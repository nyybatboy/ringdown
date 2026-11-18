package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/mfbonfigli/dialout/internal/audio"
	"github.com/mfbonfigli/dialout/internal/voice"
)

// Session is one Gemini Live conversation. The reader goroutine turns server messages into voice
// events; the methods that send are safe to call from any goroutine.
type Session struct {
	ctx context.Context
	ws  *websocket.Conn

	events    chan voice.Event
	abort     chan struct{}
	done      chan struct{}
	err       error
	ended     atomic.Bool
	endOnce   sync.Once
	closeOnce sync.Once

	// Reader state.
	dec        *decimator
	odd        []byte // a byte left over from a blob with an odd length
	speaking   bool   // agent audio since the last turn end or interruption
	agentText  strings.Builder
	calleeText strings.Builder
	usage      voice.Usage

	Trace func(kind string, raw []byte) // every server message, for probes; nil: none
}

func newSession(ctx context.Context, ws *websocket.Conn) *Session {
	s := &Session{
		ctx: ctx, ws: ws, dec: newDecimator(),
		events: make(chan voice.Event, 64), abort: make(chan struct{}), done: make(chan struct{}),
		usage: voice.Usage{},
	}
	go s.read()
	return s
}

func (s *Session) Events() <-chan voice.Event { return s.events }

// SendAudio streams the callee's 8 kHz PCM as little-endian 16-bit.
func (s *Session) SendAudio(pcm []int16) error {
	return s.send(map[string]any{"realtimeInput": map[string]any{"audio": map[string]any{
		"data": base64.StdEncoding.EncodeToString(audio.PCMToLE16(nil, pcm)), "mimeType": "audio/pcm;rate=8000",
	}}})
}

// Cue sends the system's line as realtime text. Gemini answers text it gets this way, so a cue
// without respond is marked as context for the next turn.
func (s *Session) Cue(text string, respond bool) error {
	if !respond {
		text = "(Context for your next turn, not a request to speak now) " + text
	}
	return s.send(map[string]any{"realtimeInput": map[string]any{"text": text}})
}

// EndWait is how long End leaves the connection open for the caller's last words to be transcribed.
var EndWait = 3 * time.Second

// End stops sending and closes the conversation. agent-phone fork: Gemini transcribes a caller's
// words only once their turn ends, so a caller who hung up right after speaking lost their message
// (a live test, 2026-11-16). End sends 800 ms of silence and audioStreamEnd, then waits EndWait
// for the transcription (the reader flushes what came) before closing.
func (s *Session) End() error {
	s.endOnce.Do(func() {
		s.ended.Store(true)
		go func() {
			silence := base64.StdEncoding.EncodeToString(audio.PCMToLE16(nil, make([]int16, 6400)))
			_ = writeJSON(s.ctx, s.ws, map[string]any{"realtimeInput": map[string]any{"audio": map[string]any{
				"data": silence, "mimeType": "audio/pcm;rate=8000"}}})
			_ = writeJSON(s.ctx, s.ws, map[string]any{"realtimeInput": map[string]any{"audioStreamEnd": true}})
			select {
			case <-time.After(EndWait):
			case <-s.done:
			case <-s.abort:
			}
			s.ws.Close(websocket.StatusNormalClosure, "call over")
		}()
	})
	return nil
}

func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.ended.Store(true)
		close(s.abort)
		s.ws.CloseNow()
	})
	<-s.done
	return s.err
}

func (s *Session) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

func (s *Session) send(v any) error {
	if s.ended.Load() {
		return voice.ErrClosed
	}
	if err := writeJSON(s.ctx, s.ws, v); err != nil {
		if s.ended.Load() {
			return voice.ErrClosed
		}
		return err
	}
	return nil
}

func (s *Session) aborted() bool {
	select {
	case <-s.abort:
		return true
	default:
		return false
	}
}

func (s *Session) read() {
	defer close(s.events)
	defer close(s.done)
	// agent-phone fork: words still buffered when the connection ends (a caller who hangs up right
	// after speaking, before turnComplete) go out as transcript lines, unless the session was aborted.
	defer func() {
		if !s.aborted() {
			_ = s.flushCallee()
			_ = s.flushAgent()
		}
	}()
	for {
		_, data, err := s.ws.Read(s.ctx)
		if err != nil {
			if !(s.aborted() || s.ended.Load() || websocket.CloseStatus(err) == websocket.StatusNormalClosure) {
				var ce websocket.CloseError
				if errors.As(err, &ce) {
					s.err = fmt.Errorf("gemini: connection closed (%d): %s", ce.Code, ce.Reason)
				} else {
					s.err = fmt.Errorf("gemini: connection closed: %w", err)
				}
			}
			return
		}
		if err := s.handle(data); err != nil {
			if !errors.Is(err, voice.ErrClosed) {
				s.err = err
			}
			return
		}
	}
}

func (s *Session) emit(e voice.Event) error {
	select {
	case s.events <- e:
		return nil
	case <-s.abort:
		return voice.ErrClosed
	}
}

type message struct {
	ServerContent *struct {
		ModelTurn *struct {
			Parts []struct {
				InlineData *struct {
					Data string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"modelTurn"`
		InputTranscription  *struct{ Text string } `json:"inputTranscription"`
		OutputTranscription *struct{ Text string } `json:"outputTranscription"`
		GenerationComplete  bool                   `json:"generationComplete"`
		TurnComplete        bool                   `json:"turnComplete"`
		Interrupted         bool                   `json:"interrupted"`
	} `json:"serverContent"`
	ToolCall *struct {
		FunctionCalls []struct {
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"functionCalls"`
	} `json:"toolCall"`
	UsageMetadata *struct {
		PromptTokenCount   int `json:"promptTokenCount"`
		ResponseTokenCount int `json:"responseTokenCount"`
		TotalTokenCount    int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

func (s *Session) handle(data []byte) error {
	var m message
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("gemini: decoding a message: %w", err)
	}
	if s.Trace != nil {
		s.Trace(kindOf(data), data)
	}
	if sc := m.ServerContent; sc != nil {
		if t := sc.InputTranscription; t != nil {
			s.calleeText.WriteString(t.Text)
		}
		if t := sc.OutputTranscription; t != nil {
			s.agentText.WriteString(t.Text)
		}
		if sc.Interrupted {
			if err := s.flushAgent(); err != nil {
				return err
			}
			if s.speaking {
				s.speaking = false
				s.dec = newDecimator()
				s.odd = nil
				if err := s.emit(voice.Interrupted{}); err != nil {
					return err
				}
			}
		}
		if mt := sc.ModelTurn; mt != nil {
			for _, p := range mt.Parts {
				if p.InlineData == nil || p.InlineData.Data == "" {
					continue
				}
				b, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
				if err != nil {
					return fmt.Errorf("gemini: decoding audio: %w", err)
				}
				if !s.speaking {
					// The agent starts its turn: what the callee said before it is final.
					if err := s.flushCallee(); err != nil {
						return err
					}
				}
				s.speaking = true
				if len(s.odd) > 0 {
					b = append(s.odd, b...)
					s.odd = nil
				}
				if len(b)%2 == 1 {
					s.odd = []byte{b[len(b)-1]}
					b = b[:len(b)-1]
				}
				if pcm := s.dec.process(audio.PCMFromLE16(nil, b)); len(pcm) > 0 {
					if err := s.emit(voice.AudioOut{PCM: pcm}); err != nil {
						return err
					}
				}
			}
		}
		// generationComplete: all the turn's audio is delivered (turnComplete comes only once it
		// would have finished playing, seconds later). That is voice.TurnEnd.
		if sc.GenerationComplete && s.speaking {
			s.speaking = false
			if err := s.emit(voice.TurnEnd{}); err != nil {
				return err
			}
		}
		if sc.TurnComplete {
			if err := s.flushCallee(); err != nil {
				return err
			}
			if err := s.flushAgent(); err != nil {
				return err
			}
			if s.speaking {
				s.speaking = false
				if err := s.emit(voice.TurnEnd{}); err != nil {
					return err
				}
			}
		}
	}
	if tc := m.ToolCall; tc != nil {
		var responses []map[string]any
		for _, fc := range tc.FunctionCalls {
			args := fc.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			ev, perr := voice.ParseTool(fc.Name, args)
			var out map[string]any
			_ = json.Unmarshal([]byte(voice.PlainToolResult(perr)), &out)
			responses = append(responses, map[string]any{"id": fc.ID, "name": fc.Name, "response": out})
			if perr == nil {
				if err := s.emit(ev); err != nil {
					return err
				}
			}
		}
		if err := s.send(map[string]any{"toolResponse": map[string]any{"functionResponses": responses}}); err != nil &&
			!errors.Is(err, voice.ErrClosed) {
			return err
		}
	}
	if u := m.UsageMetadata; u != nil {
		s.usage["prompt_tokens"] = u.PromptTokenCount
		s.usage["response_tokens"] = u.ResponseTokenCount
		s.usage["total_tokens"] = u.TotalTokenCount
		snap := voice.Usage{}
		for k, v := range s.usage {
			snap[k] = v
		}
		return s.emit(snap)
	}
	return nil
}

func (s *Session) flushCallee() error {
	t := cleanTranscript(s.calleeText.String())
	s.calleeText.Reset()
	if t == "" {
		return nil
	}
	return s.emit(voice.Transcript{Speaker: voice.SpeakerCallee, Text: t})
}

func (s *Session) flushAgent() error {
	t := cleanTranscript(s.agentText.String())
	s.agentText.Reset()
	if t == "" {
		return nil
	}
	return s.emit(voice.Transcript{Speaker: voice.SpeakerAgent, Text: t})
}

// kindOf names a server message by its top-level keys, for traces.
func kindOf(data []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return "?"
	}
	var keys []string
	for k, v := range m {
		if k == "serverContent" {
			var sc map[string]json.RawMessage
			if json.Unmarshal(v, &sc) == nil {
				for k2 := range sc {
					keys = append(keys, "serverContent."+k2)
				}
				continue
			}
		}
		keys = append(keys, k)
	}
	return strings.Join(keys, ",")
}

// markerRE matches the transcriber's own markers, e.g. "<no speech>" and "{pause}", which aren't
// words anyone said (agent-phone fork; seen on a live test call, 2026-11-16).
var markerRE = regexp.MustCompile(`<[a-z][a-z _-]{0,28}>|\{[a-z][a-z _-]{0,28}\}`)

func cleanTranscript(t string) string {
	return strings.Join(strings.Fields(markerRE.ReplaceAllString(t, " ")), " ")
}
