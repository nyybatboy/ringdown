// Package gemini holds the conversation on Google's Gemini Live API (BidiGenerateContent over a
// server-side WebSocket) behind voice.Engine. agent-phone fork (2026-11-16).
//
// Audio: the line's 8 kHz PCM goes in as audio/pcm;rate=8000 (Google resamples; SaySo has run
// this way in production since 2026-03); the agent's 24 kHz PCM comes back and is filtered down
// to 8 kHz here. Instructions, voice, tools, turn detection and transcription go with every
// session's setup message.
//
// Facts read from Google's docs on 2026-11-16 (aistudio.google.com/docs/models/gemini-3.8-live):
// proactive audio is always on and may not be set; thinking and affective-dialog settings are
// rejected on gemini-3.8-live; text mid-session goes as realtimeInput.text.
package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/prompts"
)

const (
	DefaultBaseURL    = "wss://generativelanguage.googleapis.com"
	DefaultModel      = "gemini-3.8-live"
	DefaultAPIVersion = "v1beta"
	DefaultVoice      = "Achird" // picked by ear in a phone-band listening test, 2026-11-16 (alternate: Zubenelgenubi)
)

// voices maps Gemini's prebuilt voices to their grammatical gender (true: female). A voice
// missing here needs Engine.Female.
var voices = map[string]bool{
	"Zephyr": true, "Puck": false, "Charon": false, "Kore": true, "Fenrir": false, "Leda": true, "Orus": false,
	"Aoede": true, "Callirrhoe": true, "Autonoe": true, "Enceladus": false, "Iapetus": false, "Umbriel": false,
	"Algieba": false, "Despina": true, "Erinome": true, "Algenib": false, "Rasalgethi": false, "Laomedeia": true,
	"Achernar": true, "Alnilam": false, "Schedar": false, "Gacrux": true, "Pulcherrima": true, "Achird": false,
	"Zubenelgenubi": false, "Vindemiatrix": true, "Sadachbia": false, "Sadaltager": false, "Sulafat": true,
}

var (
	_ voice.Engine  = (*Engine)(nil)
	_ voice.Session = (*Session)(nil)
)

// Engine opens Gemini Live sessions.
type Engine struct {
	APIKey     string
	Model      string // DefaultModel when empty
	BaseURL    string // DefaultBaseURL when empty
	APIVersion string // DefaultAPIVersion when empty
	Voice      string // DefaultVoice when a call names none
	Female     *bool  // the gender of a voice missing from the list
	// SilenceMs is the end-of-turn silence for Google's activity detection; 0: Google's default.
	SilenceMs int
}

func (*Engine) Name() string { return "gemini" }

func (e *Engine) Prepare(c voice.Config) (voice.Config, error) { return e.prompt(c) }

func (e *Engine) prompt(c voice.Config) (voice.Config, error) {
	if e.APIKey == "" {
		return c, errors.New("gemini: an API key is required")
	}
	if c.Voice == "" {
		c.Voice = e.Voice
	}
	if c.Voice == "" {
		c.Voice = DefaultVoice
	}
	female, known := voices[c.Voice]
	if e.Female != nil {
		female, known = *e.Female, true
	}
	if !known {
		return c, fmt.Errorf("gemini: voice %q isn't a known one: set its gender too", c.Voice)
	}
	return c.Render(prompts.WithGoodbye, female)
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Dial opens the WebSocket, sends the setup and waits for setupComplete. The key goes in a
// header, never the URL.
func (e *Engine) Dial(ctx context.Context, c voice.Config) (voice.Session, error) {
	if c.Prompt == nil {
		var err error
		if c, err = e.prompt(c); err != nil {
			return nil, err
		}
	}
	base := strings.TrimRight(or(e.BaseURL, DefaultBaseURL), "/")
	url := base + "/ws/google.ai.generativelanguage." + or(e.APIVersion, DefaultAPIVersion) +
		".GenerativeService.BidiGenerateContent"
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"x-goog-api-key": {e.APIKey}}})
	if err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}
	ws.SetReadLimit(16 << 20)
	if err := writeJSON(ctx, ws, e.setup(c)); err != nil {
		ws.CloseNow()
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		_, data, err := ws.Read(cctx)
		if err != nil {
			if cs := websocket.CloseStatus(err); cs != -1 {
				var ce websocket.CloseError
				errors.As(err, &ce)
				return nil, fmt.Errorf("gemini: setup refused (%d): %s", cs, ce.Reason)
			}
			return nil, fmt.Errorf("gemini: waiting for setup: %w", err)
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(data, &m) == nil {
			if _, ok := m["setupComplete"]; ok {
				return newSession(ctx, ws), nil
			}
		}
	}
}

func (e *Engine) setup(c voice.Config) map[string]any {
	var decls []map[string]any
	for _, t := range voice.ToolSpecs(c.Prompt.Tools) {
		decls = append(decls, map[string]any{
			"name": t.Name, "description": t.Description, "parametersJsonSchema": json.RawMessage(t.Schema),
		})
	}
	aad := map[string]any{
		"disabled":                 false,
		"startOfSpeechSensitivity": "START_SENSITIVITY_LOW", // line noise isn't speech
		"endOfSpeechSensitivity":   "END_SENSITIVITY_HIGH",  // answer promptly
		"prefixPaddingMs":          200,
	}
	if e.SilenceMs > 0 {
		aad["silenceDurationMs"] = e.SilenceMs
	}
	return map[string]any{"setup": map[string]any{
		"model": "models/" + or(e.Model, DefaultModel),
		"generationConfig": map[string]any{
			"responseModalities": []string{"AUDIO"},
			"speechConfig":       map[string]any{"voiceConfig": map[string]any{"prebuiltVoiceConfig": map[string]any{"voiceName": c.Voice}}},
		},
		"systemInstruction":        map[string]any{"parts": []map[string]any{{"text": c.Prompt.System}}},
		"tools":                    []map[string]any{{"functionDeclarations": decls}},
		"realtimeInputConfig":      map[string]any{"automaticActivityDetection": aad},
		"inputAudioTranscription":  map[string]any{},
		"outputAudioTranscription": map[string]any{},
	}}
}

func writeJSON(ctx context.Context, ws *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ws.Write(wctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("gemini: send: %w", err)
	}
	return nil
}
