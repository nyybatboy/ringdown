package server

import (
	"strings"
	"testing"

	"github.com/mfbonfigli/dialout/internal/call"
	"github.com/mfbonfigli/dialout/internal/voice"
)

// agent-phone fork: the pass rule shared by dialout smoke and dialout check.
func TestCheckLegs(t *testing.T) {
	both := []call.Line{{Speaker: voice.SpeakerAgent, Text: "Hello."}, {Speaker: voice.SpeakerCallee, Text: "Hi."}}
	good := Record{CallID: "a", Answered: true, Status: "completed", Transcript: both}
	if err := CheckLegs(good, good); err != nil {
		t.Fatalf("two answered legs with both sides: %v", err)
	}
	cases := []struct {
		name    string
		out, in Record
		want    string
	}{
		{"no inbound record", good, Record{}, "inbound: no record"},
		{"outbound not answered", Record{CallID: "a", Status: "no-answer"}, good, "outbound: not answered (no-answer)"},
		{"inbound one-sided", good, Record{CallID: "b", Answered: true, Transcript: both[:1]}, "inbound: the transcript lacks a side"},
		{"outbound error", Record{CallID: "a", Answered: true, Transcript: both, Error: "engine closed"}, good, "outbound: engine closed"},
	}
	for _, c := range cases {
		err := CheckLegs(c.out, c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", c.name, err, c.want)
		}
	}
}
