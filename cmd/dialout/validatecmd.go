package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/mfbonfigli/dialout/internal/config"
)

// cmdValidate checks a configuration file without starting anything or calling any vendor
// (agent-phone fork): the step an installing agent runs before serve, smoke or check. It also
// fails on the example files' placeholder values, which parse fine but can't place a call.
func cmdValidate(args []string) int {
	fl := flag.NewFlagSet("validate", flag.ContinueOnError)
	cfgPath := fl.String("config", "", "configuration file")
	srv := fl.Bool("server", true, "also check what serve needs (server.mcp_token)")
	allow := fl.Bool("allow-placeholders", false, "pass even with the examples' placeholder values (for testing the docs)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	path, err := config.Find(*cfgPath)
	if err != nil {
		fmt.Println("FAIL:", err)
		return 1
	}
	cfg, err := config.Load(path)
	if err == nil && *srv {
		err = cfg.CheckServer()
	}
	if err != nil {
		fmt.Printf("FAIL: %s\n%v\n", path, err)
		return 1
	}
	if ph := placeholders(cfg); len(ph) > 0 {
		if !*allow {
			fmt.Printf("FAIL: %s parses, but still has the examples' placeholder values:\n  - %s\n", path, strings.Join(ph, "\n  - "))
			return 1
		}
		fmt.Printf("placeholders allowed: %s\n", strings.Join(ph, "; "))
	}
	if strings.HasPrefix(cfg.Server.MCPListen, ":") || strings.HasPrefix(cfg.Server.MCPListen, "0.0.0.0:") {
		fmt.Printf("note: server.mcp_listen %s listens on every interface; the bearer token is its only guard. Use 127.0.0.1 unless another host or container must reach it.\n", cfg.Server.MCPListen)
	}
	fmt.Printf("PASS: %s (owner %q, number %s, %d engine profiles, inbound %v, call ceiling %d minutes)\n",
		path, cfg.Owner, cfg.Twilio.From, len(cfg.Profiles), cfg.Inbound.Enabled, config.MaxMinutes)
	return 0
}

// placeholders lists values copied from examples/ that were never replaced.
func placeholders(c *config.Config) []string {
	var out []string
	bad := func(name, v string) {
		if v == "replace-me" || strings.Trim(strings.TrimPrefix(strings.TrimPrefix(v, "AC"), "SK"), "0") == "" && v != "" {
			out = append(out, name+" is a placeholder")
		}
	}
	bad("twilio.account_sid", c.Twilio.AccountSID)
	bad("twilio.api_key_sid", c.Twilio.APIKeySID)
	bad("twilio.api_key_secret", c.Twilio.APIKeySecret)
	bad("twilio.auth_token", c.Twilio.AuthToken)
	bad("engine.gemini.api_key", c.Engine.Gemini.APIKey)
	bad("engine.grok.api_key", c.Engine.Grok.APIKey)
	h := c.Tunnel.Hostname
	if strings.Contains(h, "your-domain") || strings.HasSuffix(h, ".invalid") || strings.HasSuffix(h, "example.com") {
		out = append(out, "tunnel.hostname "+h+" is a placeholder")
	}
	if fictional(c.Twilio.From) {
		out = append(out, "twilio.from "+c.Twilio.From+" is a fictional 555-01xx number")
	}
	for _, e := range c.Policy.Allowlist {
		if fictional(e.Number) {
			out = append(out, "allowlist entry "+e.Label+" ("+e.Number+") is a fictional 555-01xx number")
		}
	}
	return out
}

// fictional reports a North American 555-01xx number, the range reserved for fiction.
func fictional(n string) bool {
	d := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, n)
	return len(d) == 11 && d[0] == '1' && d[4:7] == "555" && d[7:9] == "01"
}
