package call

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/mfbonfigli/dialout/internal/audio"
	"github.com/mfbonfigli/dialout/internal/twilio"
	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/internal/watchdog"
	"github.com/mfbonfigli/dialout/prompts"
)

const (
	// markSamples is how much agent audio goes between two marks: the recorder's
	// placement granularity, and how soon the bridge knows the agent has gone quiet.
	markSamples = 1600 // 200 ms
	// pendingFlush puts a mark after the last audio of a turn when Sonic doesn't end the turn.
	pendingFlush = 300 * time.Millisecond
	// replyWait bounds how long the bridge waits for Sonic to answer the callee before a cue may go
	// out anyway.
	replyWait = 8 * time.Second
)

// bridge carries one conversation: the callee's audio from Twilio to Sonic and the recorder, the
// agent's audio from Sonic to Twilio (with marks) and the recorder, and the cues and hang-up rules.
// Its loop owns all the state; the Twilio reader only forwards.
type bridge struct {
	r         *run
	ws        *websocket.Conn
	streamSID string
	sess      voice.Session

	marks    chan string   // echoed marks, from the reader
	spoke    chan struct{} // the callee has started talking
	readDone chan error    // the reader stopped: nil for Twilio's stop message

	pending     []int16 // agent audio sent to Twilio and not yet followed by a mark
	outstanding map[string]bool
	markSeq     int
	lastAudio   time.Time
	agentSpoke  bool
	calleeSpoke bool

	lastCallee  time.Time // the callee's last transcript line
	lastTurnEnd time.Time // the end of the agent's last turn

	wrapDue, wrapSent bool
	outcomeAt         time.Time
	endCallAt         time.Time
	audioAfterEnd     bool
	turnEndAfterEnd   bool
	goodbyeSaid       bool // end_call came right after the agent's goodbye

	overtimeAt        time.Time // the time limit was reached
	finalAt           time.Time // the final cue went out
	turnEndAfterFinal bool

	closedAt time.Time // the engine closed the conversation cleanly (ElevenLabs' own end_call)

	// The hang-up guard (ringdown fork): callee speech after end_call holds the hang-up until
	// its transcript says whether it's a goodbye (hang up) or more conversation (the call goes on).
	calleeAfterEnd time.Time

	// The watchdog (Config.Watchdog): a check every few transcript lines, the latest only.
	verdicts  chan verdict
	wdRules   watchdog.Rules
	wdCancel  context.CancelFunc
	wdLines   int // transcript lines since the last check
	wdStarted int // checks started
}

type verdict struct {
	answers watchdog.Answers
	err     error
}

func newBridge(r *run, ws *websocket.Conn, streamSID string, sess voice.Session) *bridge {
	b := &bridge{
		r: r, ws: ws, streamSID: streamSID, sess: sess,
		marks:       make(chan string, 64),
		spoke:       make(chan struct{}, 1),
		readDone:    make(chan error, 1),
		outstanding: map[string]bool{},
		verdicts:    make(chan verdict, 1),
	}
	if w := r.cfg.Watchdog; w != nil {
		b.wdRules = watchdog.Rules{T: w.Thresholds}
		r.res.Watchdog = &WatchdogReport{}
	}
	return b
}

// run returns why the conversation ended.
func (b *bridge) run(ctx context.Context) string {
	rctx, stopReader := context.WithCancel(context.WithoutCancel(ctx))
	defer stopReader()
	go b.read(rctx)
	defer func() {
		if b.wdCancel != nil {
			b.wdCancel()
		}
	}()

	t := b.r.t
	tick := time.NewTicker(t.Tick)
	defer tick.Stop()
	openingCue, openAfter := b.r.vc.Prompt.Cues.Opening, t.OpeningCue
	if b.r.p.Inbound != nil {
		// They called: the one who picks up speaks first.
		openingCue, openAfter = b.r.vc.Prompt.Cues.Answer, t.AnswerCue
	}
	opening := time.NewTimer(openAfter)
	defer opening.Stop()
	var rescue <-chan time.Time // armed when the opening cue was skipped for the callee's hello
	wrap := time.NewTimer(b.r.p.MaxDuration - t.WrapUp)
	defer wrap.Stop()
	limit := time.NewTimer(b.r.p.MaxDuration)
	defer limit.Stop()
	events := b.sess.Events()

	for {
		select {
		case <-ctx.Done():
			return "hung_up_on_request"
		case err := <-b.readDone:
			if err == nil {
				return "callee_hung_up"
			}
			return "stream_closed: " + err.Error()
		case <-b.spoke:
			b.calleeSpoke = true
			if !b.endCallAt.IsZero() && b.calleeAfterEnd.IsZero() {
				b.calleeAfterEnd = time.Now()
				b.r.observe("status", "hang-up held: the callee is talking")
			}
		case name := <-b.marks:
			delete(b.outstanding, name)
		case ev, ok := <-events:
			if !ok {
				if err := b.sess.Err(); err != nil {
					b.r.res.Err = fmt.Errorf("call: %w", err) // engine errors carry the engine's name
					return "engine_error"
				}
				// A clean close is the agent ending the conversation, e.g. with ElevenLabs' own
				// end_call: stop reading, let the audio already sent play, then hang up.
				events = nil
				b.closedAt = time.Now()
				b.mark(ctx)
				b.r.observe("status", "the agent closed the conversation")
				continue
			}
			b.handle(ctx, ev)
		case v := <-b.verdicts:
			if reason := b.onVerdict(time.Now(), v); reason != "" {
				return reason
			}
		case <-opening.C:
			switch {
			case b.agentSpoke:
			case !b.calleeSpoke || b.r.p.Inbound != nil:
				b.cue(openingCue, true)
			case t.OpeningRescue > 0:
				rescue = time.After(t.OpeningRescue)
			}
		case <-rescue:
			// The callee said hello and the engine never answered it.
			if !b.agentSpoke {
				b.r.observe("status", "opening rescue: the agent didn't answer the hello")
				b.cue(openingCue, true)
			}
		case <-wrap.C:
			b.wrapDue = true
		case now := <-limit.C:
			b.overtimeAt = now
		case now := <-tick.C:
			if len(b.pending) > 0 && now.Sub(b.lastAudio) >= pendingFlush {
				b.mark(ctx)
			}
			if b.wrapDue && !b.wrapSent && b.cueSafe(now) {
				b.wrapSent = true
				b.cue(b.r.vc.Prompt.Cues.WrapUp, false)
			}
			if !b.closedAt.IsZero() && (b.silent(now) || now.Sub(b.closedAt) >= t.EndMax) {
				return "agent_closed"
			}
			if reason := b.closingDue(now); reason != "" {
				return reason
			}
			if reason := b.overtime(now); reason != "" {
				return reason
			}
		}
	}
}

// read forwards Twilio's messages: the callee's audio straight to the recorder and Sonic (no hop
// through the loop), marks and the start of speech to the loop.
func (b *bridge) read(ctx context.Context) {
	var v vad
	for {
		_, data, err := b.ws.Read(ctx)
		if err != nil {
			b.readDone <- err
			return
		}
		m, err := twilio.ParseInbound(data)
		if err != nil {
			b.r.log().Debug("calls: bad stream message", "err", err)
			continue
		}
		switch m.Event {
		case "media":
			if m.Media.Track != "" && m.Media.Track != "inbound" {
				continue
			}
			mu, err1 := m.Media.Audio()
			ts, err2 := m.Media.TimestampMillis()
			if err1 != nil || err2 != nil {
				continue
			}
			pcm := audio.MulawToPCM(nil, mu)
			b.r.rec.Callee(ts, pcm)
			_ = b.sess.SendAudio(pcm) // a closed session shows up in the loop
			if v.speech(pcm) {
				select {
				case b.spoke <- struct{}{}:
				default:
				}
			}
		case "mark":
			b.r.rec.AgentPlayed(m.Mark.Name)
			select {
			case b.marks <- m.Mark.Name:
			case <-ctx.Done():
				return
			}
		case "stop":
			b.readDone <- nil
			return
		}
	}
}

func (b *bridge) handle(ctx context.Context, ev voice.Event) {
	now := time.Now()
	switch ev := ev.(type) {
	case voice.AudioOut:
		pcm := ev.PCM
		for len(pcm) > 0 {
			n := min(len(pcm), markSamples-len(b.pending))
			b.send(ctx, twilio.OutMedia(b.streamSID, audio.PCMToMulaw(nil, pcm[:n])))
			b.pending = append(b.pending, pcm[:n]...)
			pcm = pcm[n:]
			if len(b.pending) >= markSamples {
				b.mark(ctx)
			}
		}
		b.lastAudio = now
		b.agentSpoke = true
		if !b.endCallAt.IsZero() {
			b.audioAfterEnd = true
		}
	case voice.TurnEnd:
		b.mark(ctx)
		b.lastTurnEnd = now
		if !b.endCallAt.IsZero() {
			b.turnEndAfterEnd = true
		}
		if !b.finalAt.IsZero() {
			b.turnEndAfterFinal = true
		}
	case voice.Interrupted:
		// Drop what Twilio still has queued. The recorder keeps what was heard up to now. With
		// nothing playing there is nothing to cut: engines like OpenAI's signal the callee's speech
		// on every turn.
		if len(b.pending) == 0 && len(b.outstanding) == 0 {
			return
		}
		if len(b.pending) > 0 {
			b.markSeq++
			b.r.rec.AgentQueued(fmt.Sprintf("m%d", b.markSeq), b.pending)
			b.pending = nil
		}
		b.r.rec.AgentCleared()
		b.send(ctx, twilio.OutClear(b.streamSID))
		clear(b.outstanding)
		b.r.observe("interrupted", "")
	case voice.Transcript:
		b.transcript(ev)
		b.wdLines++
		b.watch(ctx)
	case voice.Outcome:
		b.r.res.Outcome = &ev
		b.outcomeAt = now
		b.r.observe("outcome", ev.Result+": "+ev.Summary)
	case voice.EndCall:
		if b.endCallAt.IsZero() {
			b.endCallAt = now
			// Engines with a full language model say goodbye and call end_call in the same turn:
			// if the agent spoke last, the goodbye is said. Sonic calls end_call first and says
			// goodbye after, so its last turn isn't the goodbye: wait for it.
			b.goodbyeSaid = b.r.vc.Closing == prompts.WithGoodbye &&
				!b.lastTurnEnd.IsZero() && b.lastTurnEnd.After(b.lastCallee)
			b.r.observe("status", "end_call: "+ev.Reason)
		}
	case voice.Usage:
		b.r.res.Usage = ev
	}
}

func (b *bridge) transcript(ev voice.Transcript) {
	line := Line{T: float64(time.Since(b.r.start).Round(100*time.Millisecond)) / float64(time.Second), Speaker: ev.Speaker, Text: ev.Text}
	b.r.res.Transcript = append(b.r.res.Transcript, line)
	if ev.Speaker == voice.SpeakerCallee {
		b.lastCallee = time.Now()
		if !b.endCallAt.IsZero() {
			b.guard(ev.Text)
		}
	}
	if err := b.r.rec.Transcript(ev.Speaker, ev.Text); err != nil {
		b.r.log().Warn("calls: transcript", "err", err)
	}
	b.r.observe(ev.Speaker, ev.Text)
}

// guard decides a held hang-up on what the callee said after end_call: a goodbye lets the call
// end; anything else cancels the end and tells the agent the call goes on.
func (b *bridge) guard(text string) {
	if goodbye(text) {
		b.calleeAfterEnd = time.Time{}
		return
	}
	b.r.observe("status", "hang-up cancelled: the callee is still talking")
	b.endCallAt, b.calleeAfterEnd = time.Time{}, time.Time{}
	b.audioAfterEnd, b.turnEndAfterEnd, b.goodbyeSaid = false, false, false
	b.cue(b.r.vc.Prompt.Cues.NotOver, false)
}

// askedLast reports whether the transcript ends on the agent's question.
func (b *bridge) askedLast() bool {
	tr := b.r.res.Transcript
	if len(tr) == 0 {
		return false
	}
	l := tr[len(tr)-1]
	return l.Speaker == voice.SpeakerAgent && strings.HasSuffix(strings.TrimSpace(l.Text), "?")
}

// goodbye reports whether a short callee line closes the call: a farewell or thanks, few words.
func goodbye(text string) bool {
	t := strings.ToLower(text)
	if len(strings.Fields(t)) > 8 {
		return false
	}
	for _, w := range []string{"bye", "thank", "thanks", "see you", "talk soon", "take care", "cheers", "sounds good", "perfect", "great", "okay", "ok", "alright", "all right"} {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// mark follows the pending agent audio with a mark, so Twilio reports when it has played.
func (b *bridge) mark(ctx context.Context) {
	if len(b.pending) == 0 {
		return
	}
	b.markSeq++
	name := fmt.Sprintf("m%d", b.markSeq)
	b.r.rec.AgentQueued(name, b.pending) // before the mark goes out, so its echo finds it
	b.pending = nil
	b.outstanding[name] = true
	b.send(ctx, twilio.OutMark(b.streamSID, name))
}

func (b *bridge) send(ctx context.Context, msg []byte) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := b.ws.Write(wctx, websocket.MessageText, msg); err != nil {
		b.r.log().Debug("calls: writing to the stream", "err", err) // the reader sees the closure
	}
}

func (b *bridge) cue(text string, respond bool) {
	if !b.closedAt.IsZero() {
		return // the agent already closed the conversation
	}
	b.r.observe("cue", text)
	if err := b.sess.Cue(text, respond); err != nil {
		b.r.log().Warn("calls: cue", "err", err)
	}
}

// silent reports whether the callee has heard all the agent's audio and none has come for a while,
// so a cue won't cut the agent off (Sonic takes text over audible speech as a barge-in).
func (b *bridge) silent(now time.Time) bool {
	return len(b.outstanding) == 0 && len(b.pending) == 0 && now.Sub(b.lastAudio) >= b.r.t.AgentQuiet
}

// cueSafe reports whether a cue can go out without cutting into the conversation: the agent is
// silent and isn't about to answer the callee. A cue sent while Sonic was preparing its answer
// interrupted it in the first test calls, and the agent then said its recap twice.
func (b *bridge) cueSafe(now time.Time) bool {
	owesReply := b.lastCallee.After(b.lastTurnEnd) && now.Sub(b.lastCallee) < replyWait
	return b.silent(now) && !owesReply
}

// overtime closes the call politely at the time limit: the final cue goes out as soon as the agent
// is silent (or after a third of the grace period anyway), the line drops once the agent has
// answered it, and at the end of the grace period in any case. Twilio's TimeLimit stays above.
func (b *bridge) overtime(now time.Time) string {
	if b.overtimeAt.IsZero() {
		return ""
	}
	grace := b.r.t.Overtime
	if b.finalAt.IsZero() && (b.silent(now) || now.Sub(b.overtimeAt) >= grace/3) {
		b.finalAt = now
		b.wrapSent = true
		b.cue(b.r.vc.Prompt.Cues.Final, true)
	}
	if (b.turnEndAfterFinal && b.silent(now)) || now.Sub(b.overtimeAt) >= grace {
		return "time_limit"
	}
	return ""
}

// watch starts a watchdog check every few transcript lines (the first after two, to catch a
// voicemail early), superseding the one still running. Not once the call is closing.
func (b *bridge) watch(ctx context.Context) {
	w := b.r.cfg.Watchdog
	if w == nil || !b.endCallAt.IsZero() || !b.overtimeAt.IsZero() || !b.closedAt.IsZero() {
		return
	}
	due := b.wdLines >= w.Every || (b.wdStarted == 0 && b.wdLines >= 2)
	if !due {
		return
	}
	b.wdLines = 0
	b.wdStarted++
	if b.wdCancel != nil {
		b.wdCancel()
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.Timeout)
	b.wdCancel = cancel
	state, qs := b.watchState(), b.r.vc.Prompt.Watchdog
	go func() {
		defer cancel()
		a, err := w.Judge.Judge(cctx, state, qs)
		if errors.Is(cctx.Err(), context.Canceled) {
			return // superseded by a newer check, or the call is over
		}
		v := verdict{a, err}
		for {
			select {
			case b.verdicts <- v:
				return
			default: // drop the unread older verdict
				select {
				case <-b.verdicts:
				default:
				}
			}
		}
	}()
}

func (b *bridge) watchState() watchdog.State {
	br := b.r.vc.Brief
	st := watchdog.State{
		Note: watchdog.StateNote, Owner: b.r.cfg.Owner, Callee: br.Callee, Kind: br.Kind,
		Objective: br.Objective, Limits: br.Limits, Context: br.Context,
		ElapsedSeconds: int(time.Since(b.r.start) / time.Second),
	}
	for _, l := range b.r.res.Transcript {
		st.Transcript = append(st.Transcript, watchdog.Line{Speaker: l.Speaker, Text: l.Text})
	}
	return st
}

// onVerdict applies a watchdog verdict. It returns a reason to hang up now, or "".
func (b *bridge) onVerdict(now time.Time, v verdict) string {
	rep := b.r.res.Watchdog
	rep.Checks++
	if v.err != nil {
		rep.Errors++
		b.r.log().Warn("calls: watchdog", "err", v.err)
		return ""
	}
	rep.Cost += v.answers.Cost
	action, why := b.wdRules.Decide(v.answers.Probabilities)
	if action != watchdog.HangUp {
		return ""
	}
	rep.Actions = append(rep.Actions, WatchdogAction{T: float64(now.Sub(b.r.start).Round(100*time.Millisecond)) / float64(time.Second),
		Action: string(action), Reason: why, Probabilities: v.answers.Probabilities})
	b.r.observe("watchdog", string(action)+": "+why)
	return "watchdog_hang_up"
}

// closingDue applies the hang-up rules after end_call or report_outcome.
func (b *bridge) closingDue(now time.Time) string {
	t := b.r.t
	if !b.endCallAt.IsZero() {
		since := now.Sub(b.endCallAt)
		// Held: the callee is talking, the agent only just stopped, or the agent's last words were
		// a question nobody has answered yet (an early test call dropped on "Can we confirm…?").
		held := !b.calleeAfterEnd.IsZero() || now.Sub(b.lastAudio) < t.HangupGrace || b.askedLast()
		switch {
		case held && since < t.EndMax:
			return ""
		case (b.turnEndAfterEnd || b.goodbyeSaid) && b.silent(now):
			return "end_call"
		case !b.audioAfterEnd && since >= t.EndNoAudio && b.silent(now):
			return "end_call_without_goodbye"
		case since >= t.EndMax:
			return "end_call_goodbye_too_long"
		}
		return ""
	}
	// ringdown fork: the callee's last words count too, so a caller still talking isn't cut off.
	// An outcome reported while the agent's question is still open (Gemini does this) waits for the
	// answer, up to EndMax (a live test, 2026-11-16: the line dropped on its question).
	if b.r.res.Outcome != nil && b.askedLast() && now.Sub(b.outcomeAt) < t.EndMax {
		return ""
	}
	if b.r.res.Outcome != nil && b.silent(now) && now.Sub(later(later(b.outcomeAt, b.lastAudio), b.lastCallee)) >= t.OutcomeIdle {
		return "outcome_without_end_call"
	}
	return ""
}

// drain handles Sonic's last events after the hang-up: transcripts, outcome and usage only.
func (b *bridge) drain(timeout time.Duration) {
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-b.sess.Events():
			if !ok {
				return
			}
			switch ev := ev.(type) {
			case voice.Transcript:
				b.transcript(ev)
			case voice.Outcome:
				if b.r.res.Outcome == nil {
					b.r.res.Outcome = &ev
				}
			case voice.Usage:
				b.r.res.Usage = ev
			}
		case <-deadline:
			return
		}
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// vad spots the first speech on the line: three 20 ms frames in a row above a level that line
// noise doesn't reach. It only decides whether the opening cue is needed; the threshold is to be
// tuned on real calls (test C1).
type vad struct{ run int }

const vadLevel = 600 // mean absolute amplitude, 16-bit scale

func (v *vad) speech(pcm []int16) bool {
	if len(pcm) == 0 {
		return false
	}
	var sum int64
	for _, s := range pcm {
		if s < 0 {
			sum -= int64(s)
		} else {
			sum += int64(s)
		}
	}
	if sum/int64(len(pcm)) > vadLevel {
		v.run++
	} else {
		v.run = 0
	}
	return v.run == 3
}
