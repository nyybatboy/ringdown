package main

// agent-phone fork: dialout check, the live check of a running install.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mfbonfigli/dialout/internal/app"
	"github.com/mfbonfigli/dialout/internal/config"
	"github.com/mfbonfigli/dialout/internal/server"
)

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// cmdCheck proves a running install end to end without stopping it: over the server's own MCP
// endpoint it places a call to the install's own number, the same server answers, and both legs
// must end with transcripts that have both sides in them. Unlike dialout smoke it takes no port
// and moves no webhook, so it is safe on a live line. It spends two short legs of call minutes.
func cmdCheck(args []string) int {
	fl := flag.NewFlagSet("check", flag.ContinueOnError)
	cfgPath := fl.String("config", "", "configuration file")
	engine := fl.String("engine", "internal", "profile or engine for the outbound leg")
	to := fl.String("to", "", "the allowlist label of the install's own number (default: the entry matching twilio.from)")
	url := fl.String("url", "", "the MCP endpoint (default: server.mcp_listen on this machine)")
	timeout := fl.Duration("timeout", 4*time.Minute, "give up after this long")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Printf("check: FAIL\n"+format+"\n", a...)
		return 1
	}
	path, err := config.Find(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check:", err)
		return 2
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check: configuration:", err)
		return 2
	}
	if _, ok := cfg.Profiles[*engine]; !ok && *engine == "internal" {
		*engine = ""
	}
	if !cfg.Inbound.Enabled {
		return fail("inbound.enabled is off, so nothing would answer the install's own number. Use dialout smoke instead.")
	}
	target := *to
	if target == "" {
		target = cfg.Twilio.From
		for _, e := range cfg.Policy.Allowlist {
			if digits(e.Number) == digits(cfg.Twilio.From) && e.Label != "" {
				target = e.Label
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// The number must already ring this install; check moves nothing.
	a, err := app.Build(cfg, app.Options{NoTunnel: true, Log: newLogger(false, slog.LevelWarn)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "check:", err)
		return 2
	}
	num, err := a.Twilio.FindNumber(ctx, cfg.Twilio.From)
	if err != nil {
		return fail("looking up %s at Twilio: %v", cfg.Twilio.From, err)
	}
	if want := "https://" + cfg.Tunnel.Hostname + server.VoicePath; num.VoiceURL != want {
		return fail("the number's webhook is %q, not %q. Point it here with: dialout number -point", num.VoiceURL, want)
	}

	endpoint := *url
	if endpoint == "" {
		host, port, err := net.SplitHostPort(cfg.Server.MCPListen)
		if err != nil {
			fmt.Fprintln(os.Stderr, "check: server.mcp_listen:", err)
			return 2
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		endpoint = "http://" + net.JoinHostPort(host, port) + "/mcp"
	}
	tr := &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: bearerTransport{cfg.Server.MCPToken}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "dialout-check", Version: version}, nil).Connect(ctx, tr, nil)
	if err != nil {
		return fail("no MCP server at %s: %v\nIs dialout serve running?", endpoint, err)
	}
	defer cs.Close()
	tool := func(name string, args map[string]any, out any) error {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if res.IsError {
			var msg []string
			for _, c := range res.Content {
				if t, ok := c.(*mcp.TextContent); ok {
					msg = append(msg, t.Text)
				}
			}
			return fmt.Errorf("%s: %s", name, strings.Join(msg, " "))
		}
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, out)
	}

	started := time.Now()
	fmt.Printf("check: calling %s from %s over %s, engine %q; this install answers itself\n", target, cfg.Twilio.From, endpoint, or(*engine, cfg.Engine.Name))
	var placed struct {
		CallID string `json:"call_id"`
	}
	if err := tool("place_call", map[string]any{"to": target, "objective": server.SmokeObjective, "max_minutes": 2,
		"engine": *engine, "callee": "whoever answers", "kind": "business", "context": server.SmokeContext,
		"request_id": fmt.Sprintf("check-%d", started.UnixNano())}, &placed); err != nil {
		return fail("%v", err)
	}
	get := func(id string) (server.Record, error) {
		var r server.Record
		for {
			if err := tool("get_call", map[string]any{"call_id": id, "wait_seconds": 120}, &r); err != nil {
				return r, err
			}
			if r.Final() {
				return r, nil
			}
			select {
			case <-ctx.Done():
				return r, errors.New("timed out waiting for " + id + " to end")
			case <-time.After(time.Second):
			}
		}
	}
	out, err := get(placed.CallID)
	if err != nil {
		return fail("%v", err)
	}
	// The answered leg is the newest inbox entry created since the check began.
	var in server.Record
	for deadline := time.Now().Add(30 * time.Second); in.CallID == "" && out.Answered && time.Now().Before(deadline); {
		var inbox struct {
			Calls []server.Record `json:"calls"`
		}
		if err := tool("list_inbox", map[string]any{"since_hours": 1}, &inbox); err != nil {
			return fail("%v", err)
		}
		for _, c := range inbox.Calls {
			if c.Created.After(started.Add(-2 * time.Second)) {
				in = c
				break
			}
		}
		if in.CallID == "" {
			time.Sleep(time.Second)
		}
	}
	if in.CallID != "" {
		if in, err = get(in.CallID); err != nil {
			return fail("%v", err)
		}
	}
	for _, leg := range []struct {
		name string
		r    server.Record
	}{{"outbound", out}, {"inbound", in}} {
		fmt.Printf("\n--- %s %s: %s, %ds, %s, $%.4f\n", leg.name, leg.r.CallID, leg.r.Status, leg.r.DurationS, leg.r.EndReason, leg.r.CostUSD)
		printLines(leg.r.Transcript)
	}
	if err := server.CheckLegs(out, in); err != nil {
		fmt.Printf("\ncheck: FAIL after %s\n%v\n", time.Since(started).Round(time.Second), err)
		return 1
	}
	fmt.Printf("\ncheck: PASS in %s\n", time.Since(started).Round(time.Second))
	return 0
}

func digits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}
