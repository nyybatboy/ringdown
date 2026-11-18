package call

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/mfbonfigli/dialout/internal/voice"
)

// agent-phone fork: the outcome check. Gemini 3.8 Live reported success while its own turn was
// still asking for the yes ("Can we confirm Thursday at 2 p.m.?") on 4 of 4 calls we watched; on
// a live call the callee never said yes. A success that ends on the agent's question with no yes
// after it is recorded as partial, and the agent's report is kept in Checked. The calling agent
// still judges the transcript itself; this keeps the record from claiming more than was agreed.

// checkOutcome returns the outcome to record for the transcript.
func checkOutcome(o *voice.Outcome, lines []Line) *voice.Outcome {
	if o == nil || o.Result != voice.ResultSuccess {
		return o
	}
	// The other side never spoke after the agent first did: nothing was confirmed (a live test,
	// 2026-11-16: a message "left" with no one acknowledging it).
	first := -1
	for i, l := range lines {
		if l.Speaker == voice.SpeakerAgent {
			first = i
			break
		}
	}
	if first >= 0 {
		// Gemini writes an agent line only when its turn completes, so a callee's yes can come first
		// in the transcript: any yes from the callee counts.
		answered := false
		for i, l := range lines {
			if l.Speaker == voice.SpeakerCallee && (i > first || affirms(l.Text)) {
				answered = true
			}
		}
		if !answered {
			c := *o
			c.Result = voice.ResultPartial
			c.Summary = "Not confirmed: the other side never answered the agent. The agent reported: " + o.Summary
			c.Checked = "reported success, but nothing the other side said came after the agent spoke"
			return &c
		}
	}
	q := -1 // the agent's last line, if it's a question
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].Speaker == voice.SpeakerAgent {
			if strings.HasSuffix(strings.TrimSpace(lines[i].Text), "?") {
				q = i
			}
			break
		}
	}
	if q < 0 || closingQuestion(lines[q].Text) {
		return o
	}
	var reply []string
	for _, l := range lines[q+1:] {
		if l.Speaker == voice.SpeakerCallee {
			reply = append(reply, l.Text)
		}
	}
	if len(reply) > 0 && affirms(strings.Join(reply, " ")) {
		return o
	}
	c := *o
	c.Result = voice.ResultPartial
	c.Summary = "Not confirmed: the agent's last question got no yes. The agent reported: " + o.Summary
	if len(reply) == 0 {
		c.Checked = fmt.Sprintf("reported success, but the call ended on the agent's question %q with no answer", lines[q].Text)
	} else {
		c.Checked = fmt.Sprintf("reported success, but the answer to the agent's question %q wasn't a yes: %q", lines[q].Text, strings.Join(reply, " "))
	}
	return &c
}

var (
	yesRE = regexp.MustCompile(`\b(yes|yeah|yep|yup|sure|okay|ok|alright|all right|fine|great|perfect|works|sounds good|deal|absolutely|definitely|of course|that's good|good)\b`)
	noRE  = regexp.MustCompile(`\b(no|nope|not|don't|can't|cannot|won't|isn't|doesn't|scared|maybe|unsure|let me think|i'll think)\b|n't\b`)
)

// affirms reports whether a callee's reply reads as a yes: a yes word and no refusal or doubt.
func affirms(text string) bool {
	t := strings.ToLower(text)
	return yesRE.MatchString(t) && !noRE.MatchString(t)
}

var closingRE = regexp.MustCompile(`\b(anything else|something else|all set|any other questions|anything i can help)\b`)

// closingQuestion reports whether the agent's question is a wrap-up check ("Anything else?"),
// which a "no" answers without undoing what was agreed before it.
func closingQuestion(text string) bool { return closingRE.MatchString(strings.ToLower(text)) }
