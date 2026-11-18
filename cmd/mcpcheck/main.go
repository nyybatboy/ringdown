// Command mcpcheck drives a running dialout serve over MCP HTTP: list_numbers, place_call to a
// label, get_call until final, list_inbox. ringdown fork: verifies the install path end to end.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8080/mcp", "MCP endpoint")
	to := flag.String("to", "", "a label to call; empty: only list")
	engine := flag.String("engine", "internal", "profile")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tr := &mcp.StreamableClientTransport{Endpoint: *url, HTTPClient: &http.Client{Transport: bearer{os.Getenv("DIALOUT_MCP_TOKEN")}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcpcheck"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(1)
	}
	defer cs.Close()
	call := func(name string, args map[string]any) map[string]any {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			fmt.Println(name, "error:", err)
			os.Exit(1)
		}
		b, _ := json.Marshal(res.StructuredContent)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if res.IsError {
			fmt.Println(name, "tool error:", res.Content)
			os.Exit(1)
		}
		return m
	}
	n := call("list_numbers", nil)
	fmt.Println("list_numbers: spend", n["spend"], "inbound", n["inbound"], "profiles", len(n["profiles"].([]any)))
	if *to == "" {
		return
	}
	p := call("place_call", map[string]any{"to": *to, "engine": *engine, "max_minutes": 2,
		"objective": "This is a short test call of the phone line. Find out who answered and whether they have a message to pass on, then say goodbye."})
	id := p["call_id"].(string)
	fmt.Println("placed", id)
	for {
		r := call("get_call", map[string]any{"call_id": id, "wait_seconds": 120})
		if s := r["status"].(string); s != "queued" && s != "ringing" && s != "in-progress" {
			fmt.Println("final:", s, r["end_reason"], "cost", r["cost_usd"], "lines", len(asSlice(r["transcript"])))
			break
		}
	}
	time.Sleep(3 * time.Second)
	in := call("list_inbox", map[string]any{"since_hours": 1})
	for _, c := range asSlice(in["calls"]) {
		m := c.(map[string]any)
		fmt.Println("inbox:", m["call_id"], m["status"], m["from"], m["end_reason"])
	}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
