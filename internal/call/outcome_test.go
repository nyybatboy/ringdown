package call

import (
	"strings"
	"testing"

	"github.com/mfbonfigli/dialout/internal/voice"
)

func lines(pairs ...string) []Line {
	var out []Line
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, Line{Speaker: pairs[i], Text: pairs[i+1]})
	}
	return out
}

const a, c = voice.SpeakerAgent, voice.SpeakerCallee

func TestCheckOutcome(t *testing.T) {
	ok := &voice.Outcome{Result: voice.ResultSuccess, Summary: "Thursday at 2."}
	for name, tc := range map[string]struct {
		lines []Line
		want  string
	}{
		// A live call: success reported while asking; no yes ever came.
		"live call 1": {lines(c, "It seems a little scary to me.", a, "I promise it's just a routine chat. Can we confirm Thursday at 2 p.m.?"), voice.ResultPartial},
		// Rehearsals: the same question, answered with a yes and a goodbye.
		"rehearsal yes": {lines(a, "Great. Does Thursday afternoon around 2 PM work for you?", c, "Great, thanks. Bye."), voice.ResultSuccess},
		"lowercase yes": {lines(a, "Would Thursday at two p.m. work for you?", c, "great, thanks, bye"), voice.ResultSuccess},
		"answered no":   {lines(a, "Does Thursday at 2 work?", c, "No, I don't think so."), voice.ResultPartial},
		"doubt":         {lines(a, "Can we lock in Thursday?", c, "Okay, maybe, I'm not sure."), voice.ResultPartial},
		"goodbye last":  {lines(c, "Yes, Thursday works.", a, "Perfect, see you Thursday. Bye!"), voice.ResultSuccess},
		"no transcript": {nil, voice.ResultSuccess},
		// a live retest: the agent left its message and hung up; nobody acknowledged it.
		"one-sided":     {lines(c, "Hello, you've reached Sam's phone.", a, "Just letting him know the test went through."), voice.ResultPartial},
		"wrap-up check": {lines(c, "Yes, Thursday at 2 works.", a, "Great, you're booked. Anything else I can help with?", c, "No, that's all, thanks."), voice.ResultSuccess},
	} {
		got := checkOutcome(ok, tc.lines)
		if got.Result != tc.want {
			t.Errorf("%s: result %s, want %s (%s)", name, got.Result, tc.want, got.Checked)
		}
		if (got.Result != voice.ResultSuccess) != (got.Checked != "") {
			t.Errorf("%s: checked = %q", name, got.Checked)
		}
	}
	if got := checkOutcome(ok, lines(a, "Can we confirm Thursday?")); !strings.HasPrefix(got.Summary, "Not confirmed:") {
		t.Errorf("downgraded summary = %q", got.Summary)
	}
	if ok.Result != voice.ResultSuccess {
		t.Error("the agent's outcome was changed in place")
	}
	partial := &voice.Outcome{Result: voice.ResultPartial}
	if checkOutcome(partial, lines(a, "Thursday?")) != partial || checkOutcome(nil, nil) != nil {
		t.Error("only a success is checked")
	}
}
