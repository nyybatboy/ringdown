package main

import (
	"flag"
	"fmt"

	"github.com/mfbonfigli/dialout/internal/config"
)

// cmdValidate checks a configuration file without starting anything or calling any vendor
// (agent-phone fork): the step an installing agent runs before serve, smoke or check.
func cmdValidate(args []string) int {
	fl := flag.NewFlagSet("validate", flag.ContinueOnError)
	cfgPath := fl.String("config", "", "configuration file")
	srv := fl.Bool("server", true, "also check what serve needs (server.mcp_token)")
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
	fmt.Printf("PASS: %s (owner %q, number %s, %d engine profiles, call ceiling %d minutes)\n",
		path, cfg.Owner, cfg.Twilio.From, len(cfg.Profiles), config.MaxMinutes)
	return 0
}
