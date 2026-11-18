package openai

// ringdown fork: xAI's Grok Voice realtime API speaks a dialect of the OpenAI Realtime
// protocol. Its session.update keeps voice, instructions and turn_detection at the session's top
// level, with the audio formats under audio.input / audio.output (docs.x.ai voice-agent page,
// read 2026-11-16); the callee's transcript comes from grok-transcribe as cumulative
// conversation.item.input_audio_transcription.updated events.

const (
	GrokBaseURL = "wss://api.x.ai"
	GrokModel   = "grok-voice-latest" // the server reports grok-voice-3.0 (2026-11-16)
	GrokVoice   = "atlas"             // picked by ear in a phone-band listening test, 2026-11-16 (alternate: rex)
)

// grokVoices is GET /v1/tts/voices on 2026-11-16: voice id to grammatical gender (true: female).
var grokVoices = map[string]bool{"altair": false, "ara": true, "atlas": false, "aurora": true, "carina": true,
	"castor": false, "celeste": true, "cosmo": false, "eve": true, "helios": false, "helix": false, "iris": true,
	"kepler": false, "leo": false, "liora": true, "lumen": false, "luna": true, "lux": false, "naksh": false,
	"orion": false, "perseus": false, "rex": false, "rigel": false, "sal": false, "sirius": false, "ursa": true,
	"zagan": false, "zenith": false}

// Grok is an Engine on xAI's realtime endpoint.
func Grok(apiKey, model, baseURL, voiceName, language string) *Engine {
	if model == "" {
		model = GrokModel
	}
	if baseURL == "" {
		baseURL = GrokBaseURL
	}
	if voiceName == "" {
		voiceName = GrokVoice
	}
	return &Engine{
		ID: "grok", APIKey: apiKey, Model: model, BaseURL: baseURL, Voice: voiceName, Language: language,
		Voices: grokVoices, XAI: true,
	}
}

// grokSession is xAI's session.update.
func grokSession(instructions, voiceName, language string, tools []map[string]any) map[string]any {
	pcmu := map[string]any{"type": "audio/pcmu", "rate": 8000}
	return map[string]any{
		"voice":          voiceName,
		"instructions":   instructions,
		"turn_detection": map[string]any{"type": "server_vad"},
		"audio": map[string]any{
			"input":  map[string]any{"format": pcmu, "transcription": map[string]any{"model": "grok-transcribe", "language_hint": language}},
			"output": map[string]any{"format": pcmu},
		},
		"tools":       tools,
		"tool_choice": "auto",
	}
}
