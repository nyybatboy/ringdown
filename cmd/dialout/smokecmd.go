package main

// agent-phone fork: dialout smoke and dialout number.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mfbonfigli/dialout/internal/app"
	"github.com/mfbonfigli/dialout/internal/call"
	"github.com/mfbonfigli/dialout/internal/config"
	"github.com/mfbonfigli/dialout/internal/server"
)

// cmdSmoke runs the loopback smoke test: the install calls its own number and answers itself.
// It points the number at this install for the test and puts the old webhook back after.
func cmdSmoke(args []string) int {
	fl := flag.NewFlagSet("smoke", flag.ContinueOnError)
	cfgPath := fl.String("config", "", "configuration file")
	engine := fl.String("engine", "internal", "profile or engine for both legs")
	to := fl.String("to", "", "the allowlist label or number of the install's own number (default: twilio.from)")
	timeout := fl.Duration("timeout", 4*time.Minute, "give up after this long")
	verbose := fl.Bool("v", false, "debug logs")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	log := newLogger(*verbose, slog.LevelWarn)
	path, err := config.Find(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		return 2
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke: configuration:", err)
		return 2
	}
	if _, ok := cfg.Profiles[*engine]; !ok && *engine == "internal" {
		*engine = ""
	}
	// The test answers with the install's inbound settings, switched on for the test only.
	override := func(c *config.Config) *config.Config {
		c.Inbound.Enabled = true
		if c.Inbound.Objective == "" {
			c.Inbound.Objective = "Find out who's calling and what they need, and take a message for " + c.Owner + "."
		}
		c.Inbound.Engine = *engine
		c.Inbound.NotifyURL = ""
		return c
	}
	cfg = override(cfg)
	a, err := app.Build(cfg, app.Options{NoTunnel: true, Log: log})
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err, "(the smoke test needs tunnel.hostname on a standing ingress)")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s, err := server.New(ctx, a)
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		return 1
	}
	s.Load = func() (*config.Config, error) {
		c, err := config.Load(path)
		if err != nil {
			return nil, err
		}
		return override(c), nil
	}
	streamsStop, err := a.Serve(s.VoiceHandler())
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		return 1
	}
	defer streamsStop()

	num, err := a.Twilio.FindNumber(ctx, cfg.Twilio.From)
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		return 1
	}
	want := "https://" + cfg.Tunnel.Hostname + server.VoicePath
	if num.VoiceURL != want {
		if _, err := a.Twilio.SetVoiceURL(ctx, num.SID, want); err != nil {
			fmt.Fprintln(os.Stderr, "smoke: pointing the number at this install:", err)
			return 1
		}
		defer func() {
			if _, err := a.Twilio.SetVoiceURL(context.Background(), num.SID, num.VoiceURL); err != nil {
				fmt.Fprintln(os.Stderr, "smoke: putting the number's webhook back to", num.VoiceURL, "failed:", err)
			}
		}()
	}

	target := *to
	if target == "" {
		target = cfg.Twilio.From
	}
	fmt.Printf("smoke: calling %s from %s, engine %q, both legs on this install\n", target, cfg.Twilio.From, or(*engine, cfg.Engine.Name))
	t0 := time.Now()
	out, in, err := s.Smoke(ctx, target, *engine, *timeout)
	stop()
	s.Wait()
	for _, leg := range []struct {
		name string
		r    server.Record
	}{{"outbound", out}, {"inbound", in}} {
		fmt.Printf("\n--- %s %s: %s, %ds, %s, $%.4f\n", leg.name, leg.r.CallID, leg.r.Status, leg.r.DurationS, leg.r.EndReason, leg.r.CostUSD)
		printLines(leg.r.Transcript)
	}
	if err != nil {
		fmt.Printf("\nsmoke: FAIL after %s\n%v\n", time.Since(t0).Round(time.Second), err)
		return 1
	}
	fmt.Printf("\nsmoke: PASS in %s\n", time.Since(t0).Round(time.Second))
	return 0
}

func printLines(lines []call.Line) {
	for _, l := range lines {
		fmt.Printf("  %5.1fs %-6s %s\n", l.T, l.Speaker, l.Text)
	}
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// cmdNumber shows the number's incoming-call webhook, or points it at this install (-point) or
// at a URL (-url).
func cmdNumber(args []string) int {
	fl := flag.NewFlagSet("number", flag.ContinueOnError)
	cfgPath := fl.String("config", "", "configuration file")
	point := fl.Bool("point", false, "point the number at https://<tunnel.hostname>/twilio/voice")
	setURL := fl.String("url", "", "point the number at this URL instead")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	path, err := config.Find(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "number:", err)
		return 2
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "number: configuration:", err)
		return 2
	}
	a, err := app.Build(cfg, app.Options{NoTunnel: cfg.Tunnel.Hostname != "", Log: newLogger(false, slog.LevelWarn)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "number:", err)
		return 2
	}
	ctx := context.Background()
	n, err := a.Twilio.FindNumber(ctx, cfg.Twilio.From)
	if err != nil {
		fmt.Fprintln(os.Stderr, "number:", err)
		return 1
	}
	u := *setURL
	if *point {
		if cfg.Tunnel.Hostname == "" {
			fmt.Fprintln(os.Stderr, "number: -point needs tunnel.hostname")
			return 2
		}
		u = "https://" + cfg.Tunnel.Hostname + server.VoicePath
	}
	if u != "" {
		if n, err = a.Twilio.SetVoiceURL(ctx, n.SID, u); err != nil {
			fmt.Fprintln(os.Stderr, "number:", err)
			return 1
		}
	}
	fmt.Printf("%s voice webhook: %s (%s)\n", n.PhoneNumber, n.VoiceURL, n.VoiceMethod)
	return 0
}
