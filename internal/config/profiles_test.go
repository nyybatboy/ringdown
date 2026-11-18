package config

import (
	"strings"
	"testing"
)

// agent-phone fork: a profile overrides its engine's settings for one call, and a bare engine name
// resolves to that engine as configured; a missing key is caught before the call.
func TestResolve(t *testing.T) {
	c := &Config{Engine: Engine{Name: "grok",
		ElevenLabs: ElevenLabs{APIKey: "k", AgentID: "agent_default", VoiceGender: "female"},
		Grok:       Grok{APIKey: "k", Voice: "rex"}},
		Profiles: map[string]Profile{"hifi": {Engine: "elevenlabs", AgentID: "agent_hifi", VoiceGender: "male"}}}
	e, _, err := c.Resolve("hifi")
	if err != nil || e.Name != "elevenlabs" || e.ElevenLabs.AgentID != "agent_hifi" || e.ElevenLabs.VoiceGender != "male" {
		t.Fatalf("hifi = %+v, %v", e, err)
	}
	if c.Engine.ElevenLabs.AgentID != "agent_default" {
		t.Fatal("Resolve changed the configuration")
	}
	if e, _, err := c.Resolve(""); err != nil || e.Name != "grok" {
		t.Fatalf("default = %s, %v", e.Name, err)
	}
	if _, _, err := c.Resolve("gemini"); err == nil {
		t.Fatal("gemini without a key accepted")
	}
	if _, _, err := c.Resolve("nope"); err == nil {
		t.Fatal("unknown engine accepted")
	}
}

func TestStandingTunnel(t *testing.T) {
	base := `
owner: Marco
twilio: {account_sid: AC1, api_key_sid: SK1, api_key_secret: s, auth_token: t, from: "+353850000000"}
policy: {enabled: true, allowlist: [{number: "+39 333 000 0000", label: M}]}
`
	if _, err := Parse([]byte(base+"tunnel: {hostname: x.ngrok-free.dev, standing: true}\n"), t.TempDir()); err != nil {
		t.Errorf("standing without a token: %v", err)
	}
	if _, err := Parse([]byte(base+"tunnel: {hostname: x.example.com}\n"), t.TempDir()); err == nil {
		t.Error("a named tunnel without its token was accepted")
	}
	if _, err := Parse([]byte(base+"tunnel: {standing: true}\n"), t.TempDir()); err == nil {
		t.Error("standing without a hostname was accepted")
	}
	if _, err := Parse([]byte(base+"inbound: {enabled: true}\n"), t.TempDir()); err == nil {
		t.Error("inbound without an objective was accepted")
	}
	spend := strings.Replace(base, "label: M}]}", "label: M}], limits: {spend_per_month_usd: 5}}", 1)
	if _, err := Parse([]byte(spend+"engine: {name: openai, openai: {api_key: k}}\n"), t.TempDir()); err == nil || !strings.Contains(err.Error(), "no rate") {
		t.Errorf("a spend limit with an engine of unknown rate: %v", err)
	}
}
