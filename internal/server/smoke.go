package server

// ringdown fork: the loopback smoke test. The install calls its own number; its inbound side
// answers; two agents talk. It passes when both legs end with a transcript that has both sides
// in it. It needs inbound (the number's webhook on this install) and spends two short legs of
// call minutes, nothing else.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mfbonfigli/dialout/internal/voice"
)

// SmokeObjective is the outbound leg's brief.
const SmokeObjective = "This is a short test call of the phone line. Find out who answered and whether they have a message to pass on, then say goodbye. Keep it under a minute."

// SmokeContext gives the test caller something to say when asked who it is (from a live review:
// without it the agent dodged the question, which reads as evasive and is a poor test of manners).
const SmokeContext = "If asked who's calling, say it's an automated test call from this phone line's own number."

// Smoke places the loopback call to `to` (the install's own number, allowlisted) and waits for
// both legs. engine is a profile or engine name for the outbound leg; empty: the default.
func (s *Server) Smoke(ctx context.Context, to, engine string, timeout time.Duration) (out, in Record, err error) {
	started := time.Now()
	_, placed, err := s.placeCall(ctx, nil, placeCallIn{To: to, Objective: SmokeObjective, MaxMinutes: 2, Engine: engine,
		Callee: "whoever answers", Kind: "business", Context: SmokeContext})
	if err != nil {
		return out, in, fmt.Errorf("placing the call: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		out, _ = s.Index.Get(placed.CallID)
		inbox := s.Index.Inbox(started.Add(-time.Second))
		in = Record{}
		if len(inbox) > 0 {
			in, _ = s.Index.Get(inbox[0].CallID)
		}
		if out.Final() && (in.CallID == "" && !out.Answered || in.Final()) {
			break
		}
		if time.Now().After(deadline) {
			return out, in, errors.New("timed out waiting for both legs to end")
		}
		select {
		case <-ctx.Done():
			return out, in, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return out, in, CheckLegs(out, in)
}

// CheckLegs is the pass rule shared by the smoke test and dialout check: both legs answered, no
// error, and each transcript has both sides in it.
func CheckLegs(out, in Record) error {
	var problems []error
	check := func(leg string, r Record) {
		if r.CallID == "" {
			problems = append(problems, fmt.Errorf("%s: no record (is the number's webhook on this install?)", leg))
			return
		}
		if !r.Answered {
			problems = append(problems, fmt.Errorf("%s: not answered (%s)", leg, r.Status))
		}
		if r.Error != "" {
			problems = append(problems, fmt.Errorf("%s: %s", leg, r.Error))
		}
		sides := map[string]int{}
		for _, l := range r.Transcript {
			sides[l.Speaker]++
		}
		if sides[voice.SpeakerAgent] == 0 || sides[voice.SpeakerCallee] == 0 {
			problems = append(problems, fmt.Errorf("%s: the transcript lacks a side (agent %d lines, callee %d)", leg,
				sides[voice.SpeakerAgent], sides[voice.SpeakerCallee]))
		}
	}
	check("outbound", out)
	check("inbound", in)
	return errors.Join(problems...)
}
