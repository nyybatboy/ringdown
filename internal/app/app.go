// Package app turns a configuration into the parts of a call: the voice engine, the Twilio
// client, the recorder, the stream endpoint and the tunnel. The local `dialout call` command and
// the MCP server share it.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mfbonfigli/dialout/internal/awscreds"
	"github.com/mfbonfigli/dialout/internal/call"
	"github.com/mfbonfigli/dialout/internal/config"
	"github.com/mfbonfigli/dialout/internal/elevenlabs"
	"github.com/mfbonfigli/dialout/internal/gemini"
	"github.com/mfbonfigli/dialout/internal/openai"
	"github.com/mfbonfigli/dialout/internal/recorder"
	"github.com/mfbonfigli/dialout/internal/sonic"
	"github.com/mfbonfigli/dialout/internal/twilio"
	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/internal/watchdog"
)

// Options change how the parts are built.
type Options struct {
	NoTunnel bool         // the hostname already reaches the stream endpoint: start no cloudflared
	Log      *slog.Logger // slog.Default() when nil
}

// App is the parts every call shares.
type App struct {
	Config   *config.Config
	Engine   voice.Engine
	Twilio   *twilio.Client
	Recorder recorder.Config
	Streams  *call.Streams
	Tunnel   call.Starter
	Watchdog *watchdog.Config // nil when off
	Log      *slog.Logger

	where func(key string) string
}

// Build checks the configuration against the outside world (the cloudflared binary, AWS
// credentials when Sonic or S3 need them) and builds the parts.
func Build(cfg *config.Config, opt Options) (*App, error) {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	a := &App{
		Config:  cfg,
		Twilio:  &twilio.Client{AccountSID: cfg.Twilio.AccountSID, KeySID: cfg.Twilio.APIKeySID, KeySecret: cfg.Twilio.APIKeySecret},
		Streams: &call.Streams{AuthToken: cfg.Twilio.AuthToken, Log: log},
		Log:     log,
	}
	var err error
	if a.Engine, err = NewEngine(cfg); err != nil {
		return nil, err
	}

	if cfg.Inbound.Enabled && !opt.NoTunnel && !cfg.Tunnel.Standing {
		return nil, errors.New("inbound.enabled needs a standing ingress: tunnel.standing and tunnel.hostname (e.g. an ngrok domain)")
	}
	switch t := cfg.Tunnel; {
	case opt.NoTunnel || t.Standing:
		if t.Hostname == "" {
			return nil, errors.New("no tunnel needs tunnel.hostname: the name that already reaches " + t.Listen)
		}
		a.Tunnel = call.NoTunnel{Host: t.Hostname}
	default:
		bin, err := call.FindCloudflared(t.Cloudflared)
		if err != nil {
			return nil, fmt.Errorf("%w; or set tunnel.cloudflared", err)
		}
		if t.Hostname != "" {
			a.Tunnel = &call.Tunnel{Binary: bin, Token: t.Token, Host: t.Hostname, Log: log}
		} else {
			a.Tunnel = &call.QuickTunnel{Binary: bin, Origin: "http://" + t.Listen, Log: log}
		}
	}

	if w := cfg.Watchdog; w.Enabled {
		a.Watchdog = &watchdog.Config{
			Judge:      &watchdog.Decisions{BaseURL: w.BaseURL, APIKey: w.APIKey, Model: w.Model},
			Thresholds: watchdog.Thresholds{HangUp: w.Thresholds.HangUp},
			Every:      w.CheckEvery,
			Timeout:    w.Timeout,
		}
	}

	var up recorder.Uploader
	if b := cfg.Storage.S3Bucket; b != "" {
		var opts []func(*awsconfig.LoadOptions) error
		if r := cfg.Storage.S3Region; r != "" {
			opts = append(opts, awsconfig.WithRegion(r))
		}
		awsCfg, err := loadAWS(opts...)
		if err != nil {
			return nil, fmt.Errorf("AWS: %w", err)
		}
		if awsCfg.Region == "" {
			return nil, errors.New("storage.s3_bucket: no region for the bucket; set storage.s3_region or AWS_REGION")
		}
		up = &recorder.S3{Client: s3.NewFromConfig(awsCfg), Bucket: b}
		a.where = func(key string) string { return "s3://" + b + "/" + key }
	} else {
		dir := &recorder.Dir{Root: cfg.Storage.Dir}
		up, a.where = dir, dir.Path
	}
	a.Recorder = recorder.Config{
		SpoolDir: cfg.Storage.Spool, KeyPrefix: cfg.Storage.S3Prefix, Segment: time.Duration(cfg.Storage.SegmentSeconds) * time.Second,
		Audio: cfg.Storage.RecordAudio, Disabled: !cfg.Storage.Enabled, Uploader: up, Log: log,
	}
	return a, nil
}

// loadAWS is the SDK's configuration, with the credentials from $DIALOUT_AWS_CREDENTIALS_FILE
// when it's set.
func loadAWS(opts ...func(*awsconfig.LoadOptions) error) (aws.Config, error) {
	if p := awscreds.FromEnv(); p != nil {
		opts = append(opts, awsconfig.WithCredentialsProvider(p))
	}
	return awsconfig.LoadDefaultConfig(context.Background(), opts...)
}

// NewEngine builds the configured default voice engine.
func NewEngine(cfg *config.Config) (voice.Engine, error) { return NewEngineFrom(cfg.Engine) }

// EngineFor builds the engine for one call from a profile or engine name (agent-phone fork:
// engine per call), with the voice that call uses. Empty: the default engine and voice.
func (a *App) EngineFor(cfg *config.Config, name string) (voice.Engine, string, error) {
	if name == "" {
		return a.Engine, a.Voice(), nil
	}
	e, _, err := cfg.Resolve(name)
	if err != nil {
		return nil, "", err
	}
	eng, err := NewEngineFrom(e)
	if err != nil {
		return nil, "", err
	}
	return eng, voiceOf(e), nil
}

// NewEngineFrom builds a voice engine from engine settings.
func NewEngineFrom(e config.Engine) (voice.Engine, error) {
	switch e.Name {
	case "sonic":
		awsCfg, err := loadAWS(awsconfig.WithRegion(e.Sonic.Region))
		if err != nil {
			return nil, fmt.Errorf("AWS: %w", err)
		}
		return sonic.NewEngine(bedrockruntime.NewFromConfig(awsCfg)), nil
	case "elevenlabs":
		el := e.ElevenLabs
		return &elevenlabs.Engine{APIKey: el.APIKey, AgentID: el.AgentID, BaseURL: el.APIBase, Voice: el.VoiceID,
			Female: el.VoiceGender != "male", LLM: el.LLM}, nil
	case "openai":
		o := e.OpenAI
		eng := &openai.Engine{APIKey: o.APIKey, Model: o.Model, BaseURL: o.APIBase, Voice: o.Voice, Language: e.Language}
		if o.VoiceGender != "" {
			female := o.VoiceGender == "female"
			eng.Female = &female
		}
		return eng, nil
	case "grok":
		g := e.Grok
		eng := openai.Grok(g.APIKey, g.Model, g.APIBase, g.Voice, e.Language)
		if g.VoiceGender != "" {
			female := g.VoiceGender == "female"
			eng.Female = &female
		}
		return eng, nil
	case "gemini":
		g := e.Gemini
		eng := &gemini.Engine{APIKey: g.APIKey, Model: g.Model, BaseURL: g.APIBase, APIVersion: g.APIVersion, Voice: g.Voice}
		if g.VoiceGender != "" {
			female := g.VoiceGender == "female"
			eng.Female = &female
		}
		return eng, nil
	}
	return nil, fmt.Errorf("unknown engine %q", e.Name)
}

// Voice is the configured voice of the default engine; empty means the engine's default.
func (a *App) Voice() string { return voiceOf(a.Config.Engine) }

func voiceOf(e config.Engine) string {
	switch e.Name {
	case "sonic":
		return e.Sonic.Voice
	case "elevenlabs":
		return e.ElevenLabs.VoiceID
	case "openai":
		return e.OpenAI.Voice
	case "grok":
		return e.Grok.Voice
	case "gemini":
		return e.Gemini.Voice
	}
	return ""
}

// Where says where a stored object ends up: a local path or an s3:// URL.
func (a *App) Where(key string) string { return a.where(key) }

// Recover finishes the calls a crash left in the spool. fix gets each call's id and meta.json.
func (a *App) Recover(ctx context.Context, fix func(callID string, meta []byte) []byte) ([]recorder.Recovered, error) {
	return recorder.Recover(ctx, a.Recorder, func(id string, meta []byte) []byte {
		meta = call.MarkInterrupted(meta)
		if fix != nil {
			meta = fix(id, meta)
		}
		return meta
	})
}

// Serve starts the stream endpoint that the tunnel forwards to. Stop it with the returned func.
// voice, when not nil, answers Twilio's incoming-call webhook at /twilio/voice on the same
// listener (agent-phone fork: inbound calls).
func (a *App) Serve(voice ...http.Handler) (stop func(), err error) {
	ln, err := net.Listen("tcp", a.Config.Tunnel.Listen)
	if err != nil {
		return nil, fmt.Errorf("stream endpoint: %w", err)
	}
	var h http.Handler = a.Streams
	if len(voice) > 0 && voice[0] != nil {
		mux := http.NewServeMux()
		mux.Handle("/twilio/voice", voice[0])
		mux.Handle("/", a.Streams)
		h = mux
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return func() { srv.Close() }, nil
}

// CallConfig is the call engine's configuration. The policy and the prompt file may be newer than
// the rest: the server re-reads them before every call.
func (a *App) CallConfig(cfg *config.Config, notify func(string), observe func(call.Event)) call.Config {
	return call.Config{
		Twilio: a.Twilio, From: a.Config.Twilio.From, Streams: a.Streams, Tunnel: a.Tunnel, Engine: a.Engine,
		Recorder: a.Recorder, Owner: a.Config.Owner, Voice: a.Voice(), Prompts: cfg.Prompts, Location: cfg.Location,
		Watchdog: a.Watchdog,
		Notify:   notify, Observe: observe, Log: a.Log,
	}
}
