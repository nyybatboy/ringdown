package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/mfbonfigli/dialout/internal/audio"
	"github.com/mfbonfigli/dialout/internal/voice"
)

// Session is one Realtime conversation. The reader goroutine turns server events into voice
// events; the methods that send are safe to call from any goroutine.
type Session struct {
	ctx context.Context
	ws  *websocket.Conn

	events    chan voice.Event
	abort     chan struct{}
	done      chan struct{}
	err       error // set by the reader before done is closed
	ended     atomic.Bool
	endOnce   sync.Once
	closeOnce sync.Once

	// Reader state.
	audioSinceStop bool             // agent audio arrived since the last Interrupted
	toolOutputs    []map[string]any // tool results to send once the response that called them is done
	endCalled      bool             // end_call came: no new response after it
	usage          voice.Usage      // running totals over the responses
	lastError      string           // the last error event, for the closing error

	// ringdown fork.
	name        string                             // the engine's name, for logs
	xai         bool                               // xAI's transcript events
	calleeItem  string                             // xAI: the item of the callee transcript being built
	calleeDraft string                             // xAI: its latest cumulative text, emitted when the next one starts or the agent answers
	Trace       func(eventType string, raw []byte) // every server event, for probes; nil: none
}

func newSession(ctx context.Context, ws *websocket.Conn) *Session {
	s := &Session{
		ctx: ctx, ws: ws,
		events: make(chan voice.Event, 64),
		abort:  make(chan struct{}),
		done:   make(chan struct{}),
		usage:  voice.Usage{},
	}
	go s.read()
	return s
}

func (s *Session) Events() <-chan voice.Event { return s.events }

// SendAudio streams the callee's audio as µ-law, the session's input format.
func (s *Session) SendAudio(pcm []int16) error {
	return s.send(map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(audio.PCMToMulaw(nil, pcm)),
	})
}

// Cue adds a user message and asks for a response when the agent should act now; otherwise a
// system message, which the agent takes into account in its next response.
func (s *Session) Cue(text string, respond bool) error {
	role := "system"
	if respond {
		role = "user"
	}
	err := s.send(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": role,
			"content": []map[string]any{{"type": "input_text", "text": text}},
		},
	})
	if err != nil || !respond {
		return err
	}
	return s.send(map[string]any{"type": "response.create"})
}

// End closes the WebSocket normally, which ends the session; Events closes once the server has
// answered the close. Idempotent.
// EndWait is how long End leaves the connection open for the caller's last words to be transcribed.
var EndWait = 3 * time.Second

// End stops sending and closes the conversation. ringdown fork: 800 ms of silence lets server
// VAD close the caller's turn, so their last words are transcribed before the close (the reader
// flushes what came); a caller who hung up right after speaking lost their message otherwise.
func (s *Session) End() error {
	s.endOnce.Do(func() {
		s.ended.Store(true)
		go func() {
			_ = writeJSON(s.ctx, s.ws, map[string]any{"type": "input_audio_buffer.append",
				"audio": base64.StdEncoding.EncodeToString(audio.PCMToMulaw(nil, make([]int16, 6400)))})
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

// Close drops the connection at once and waits for the reader to stop. Idempotent.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.ended.Store(true)
		close(s.abort) // before closing, so the end of the connection isn't taken for a failure
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
	// done closes before events (defers run last-first), so whoever sees Events close can
	// read Err right away.
	defer close(s.events)
	defer close(s.done)
	// ringdown fork: an xAI draft still pending when the connection ends is the callee's last
	// words; emit it unless the session was aborted.
	defer func() {
		if !s.aborted() {
			_ = s.flushCallee()
		}
	}()
	for {
		_, data, err := s.ws.Read(s.ctx)
		if err != nil {
			s.err = s.endErr(err)
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

// endErr says how the connection ended: a normal close, or one we asked for, is no failure.
func (s *Session) endErr(err error) error {
	if s.aborted() || s.ended.Load() || websocket.CloseStatus(err) == websocket.StatusNormalClosure {
		return nil
	}
	if s.lastError != "" {
		return fmt.Errorf("openai: connection closed after %q: %w", s.lastError, err)
	}
	return fmt.Errorf("openai: connection closed: %w", err)
}

func (s *Session) emit(e voice.Event) error {
	select {
	case s.events <- e:
		return nil
	case <-s.abort:
		return voice.ErrClosed
	}
}

type event struct {
	Type       string     `json:"type"`
	ItemID     string     `json:"item_id"`
	Delta      string     `json:"delta"`
	Transcript string     `json:"transcript"`
	Name       string     `json:"name"`
	CallID     string     `json:"call_id"`
	Arguments  string     `json:"arguments"`
	Error      *errorBody `json:"error"`
	Response   *struct {
		Usage *struct {
			InputTokenDetails struct {
				AudioTokens  int `json:"audio_tokens"`
				TextTokens   int `json:"text_tokens"`
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_token_details"`
			OutputTokenDetails struct {
				AudioTokens int `json:"audio_tokens"`
				TextTokens  int `json:"text_tokens"`
			} `json:"output_token_details"`
		} `json:"usage"`
	} `json:"response"`
}

func (s *Session) handle(data []byte) error {
	var ev event
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("openai: decoding an event: %w", err)
	}
	if s.Trace != nil {
		s.Trace(ev.Type, data)
	}
	switch ev.Type {
	case "conversation.item.input_audio_transcription.updated":
		// xAI: the transcript so far of the callee's current item.
		if ev.ItemID != s.calleeItem {
			if err := s.flushCallee(); err != nil {
				return err
			}
			s.calleeItem = ev.ItemID
		}
		s.calleeDraft = strings.TrimSpace(ev.Transcript)
		return nil
	case "response.created":
		return s.flushCallee()
	case "response.output_audio.delta", "response.audio.delta": // GA name, beta name (xAI)
		b, err := base64.StdEncoding.DecodeString(ev.Delta)
		if err != nil {
			return fmt.Errorf("openai: decoding audio: %w", err)
		}
		s.audioSinceStop = true
		return s.emit(voice.AudioOut{PCM: audio.MulawToPCM(nil, b)})

	case "response.output_audio.done", "response.audio.done":
		return s.emit(voice.TurnEnd{})

	case "input_audio_buffer.speech_started":
		// The callee started talking: the server cancels its own response; the bridge drops the
		// audio still queued, if any is playing.
		if s.audioSinceStop {
			s.audioSinceStop = false
			return s.emit(voice.Interrupted{})
		}

	case "conversation.item.input_audio_transcription.completed":
		if s.xai {
			// xAI sends .completed with every partial too: keep the latest as the draft, emitted
			// when the agent answers or the next item starts.
			if ev.ItemID != s.calleeItem {
				if err := s.flushCallee(); err != nil {
					return err
				}
				s.calleeItem = ev.ItemID
			}
			s.calleeDraft = strings.TrimSpace(ev.Transcript)
			return nil
		}
		if t := strings.TrimSpace(ev.Transcript); t != "" {
			return s.emit(voice.Transcript{Speaker: voice.SpeakerCallee, Text: t})
		}

	case "response.output_audio_transcript.done", "response.audio_transcript.done":
		// What the model said in this response. Cut by a barge-in, it can include words the
		// callee never heard: the server doesn't know what played.
		if t := strings.TrimSpace(ev.Transcript); t != "" {
			return s.emit(voice.Transcript{Speaker: voice.SpeakerAgent, Text: t})
		}

	case "response.function_call_arguments.done":
		parsed, perr := voice.ParseTool(ev.Name, json.RawMessage(ev.Arguments))
		// Only one response can be active: the result goes in, and the next response is asked
		// for, once this one is done.
		s.toolOutputs = append(s.toolOutputs, map[string]any{
			"type": "conversation.item.create",
			"item": map[string]any{"type": "function_call_output", "call_id": ev.CallID, "output": voice.PlainToolResult(perr)},
		})
		if perr == nil {
			if _, end := parsed.(voice.EndCall); end {
				s.endCalled = true
			}
			return s.emit(parsed)
		}

	case "response.done":
		if r := ev.Response; r != nil && r.Usage != nil {
			s.usage["input_audio_tokens"] += r.Usage.InputTokenDetails.AudioTokens
			s.usage["input_text_tokens"] += r.Usage.InputTokenDetails.TextTokens
			s.usage["input_cached_tokens"] += r.Usage.InputTokenDetails.CachedTokens
			s.usage["output_audio_tokens"] += r.Usage.OutputTokenDetails.AudioTokens
			s.usage["output_text_tokens"] += r.Usage.OutputTokenDetails.TextTokens
			snapshot := voice.Usage{}
			for k, v := range s.usage {
				snapshot[k] = v
			}
			if err := s.emit(snapshot); err != nil {
				return err
			}
		}
		if len(s.toolOutputs) > 0 {
			outs := s.toolOutputs
			s.toolOutputs = nil
			for _, o := range outs {
				if err := s.send(o); err != nil {
					return ignoreClosed(err)
				}
			}
			if s.endCalled {
				return nil // the goodbye is said: no new response, the call is about to end
			}
			return ignoreClosed(s.send(map[string]any{"type": "response.create"}))
		}

	case "error":
		// Most errors leave the session open: remember the last one for the closing error.
		s.lastError = ev.Error.String()
	}
	return nil
}

// flushCallee emits the callee's xAI draft transcript, if any.
func (s *Session) flushCallee() error {
	t := s.calleeDraft
	s.calleeDraft = ""
	if t == "" {
		return nil
	}
	return s.emit(voice.Transcript{Speaker: voice.SpeakerCallee, Text: t})
}

func ignoreClosed(err error) error {
	if errors.Is(err, voice.ErrClosed) {
		return nil
	}
	return err
}
