// Command probe opens one engine session without a phone line, sends the opening cue, optionally
// plays a callee line (8 kHz mono 16-bit WAV) after the agent's first turn, and prints every event
// with its time. agent-phone fork: proves an engine's protocol before any call is placed.
//
//	probe -config dialout.yaml -engine grok|gemini|<profile> [-callee line.wav] [-out agent.raw] [-trace]
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/mfbonfigli/dialout/internal/app"
	"github.com/mfbonfigli/dialout/internal/audio"
	"github.com/mfbonfigli/dialout/internal/config"
	"github.com/mfbonfigli/dialout/internal/gemini"
	"github.com/mfbonfigli/dialout/internal/openai"
	"github.com/mfbonfigli/dialout/internal/voice"
	"github.com/mfbonfigli/dialout/prompts"
)

func main() {
	cfgPath := flag.String("config", "", "dialout.yaml")
	engine := flag.String("engine", "", "engine or profile")
	calleeWav := flag.String("callee", "", "a callee line to play after the agent's first turn (8 kHz mono WAV)")
	out := flag.String("out", "", "write the agent's 8 kHz PCM here (raw s16le)")
	trace := flag.Bool("trace", false, "print every server event type")
	wait := flag.Duration("wait", 25*time.Second, "how long to run")
	voiceFlag := flag.String("voice", "", "the engine's voice (default: the profile's)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	check(err)
	a, err := app.Build(cfg, app.Options{NoTunnel: true})
	check(err)
	eng, voiceName, err := a.EngineFor(cfg, *engine)
	check(err)
	if *voiceFlag != "" {
		voiceName = *voiceFlag
	}
	vc, err := eng.Prepare(voice.Config{Owner: cfg.Owner, Voice: voiceName, Prompts: cfg.Prompts, Now: time.Now(),
		Brief: prompts.Brief{Callee: "Alex", Kind: "personal",
			Objective: "Agree a 30-minute slot this week for the phone review. Offer Wednesday or Thursday afternoon first.",
			Limits:    "Any weekday 1 to 5 pm Eastern is fine."}})
	check(err)

	ctx, cancel := context.WithTimeout(context.Background(), *wait+10*time.Second)
	defer cancel()
	t0 := time.Now()
	at := func() string { return fmt.Sprintf("%6.2fs", time.Since(t0).Seconds()) }
	sess, err := eng.Dial(ctx, vc)
	check(err)
	fmt.Println(at(), "session open:", eng.Name(), vc.Voice)
	tr := func(kind string, raw []byte) {
		if *trace {
			fmt.Println(at(), "  ev", kind)
		}
	}
	switch s := sess.(type) {
	case *openai.Session:
		s.Trace = tr
	case *gemini.Session:
		s.Trace = tr
	}

	// -out prefix: the agent's turns go to prefix-1.raw, prefix-2.raw, ...
	var sink *os.File
	newSink := func(n int) {
		if *out == "" {
			return
		}
		if sink != nil {
			sink.Close()
		}
		sink, err = os.Create(fmt.Sprintf("%s-%d.raw", *out, n))
		check(err)
	}
	newSink(1)
	defer func() {
		if sink != nil {
			sink.Close()
		}
	}()
	var firstAudioAt time.Time
	var callee []int16
	if *calleeWav != "" {
		b, err := os.ReadFile(*calleeWav)
		check(err)
		callee = audio.PCMFromLE16(nil, b[44:])
	}

	// Silence on the line, as a phone gives, then the opening cue.
	stopSilence := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopSilence) }) }
	go func() {
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stopSilence:
				return
			case <-tk.C:
				_ = sess.SendAudio(make([]int16, 160))
			}
		}
	}()
	cueAt := time.Now()
	check(sess.Cue(vc.Prompt.Cues.Opening, true))
	fmt.Println(at(), "cue sent")

	firstAudio, turns, samples := false, 0, 0
	var calleeEnd time.Time
	deadline := time.After(*wait)
	for {
		select {
		case <-deadline:
			stop()
			_ = sess.End()
			fmt.Println(at(), "done:", turns, "turns,", float64(samples)/8000, "s of agent audio")
			return
		case ev, ok := <-sess.Events():
			if !ok {
				fmt.Println(at(), "session closed, err:", sess.Err())
				return
			}
			switch e := ev.(type) {
			case voice.AudioOut:
				if !firstAudio {
					firstAudio = true
					firstAudioAt = time.Now()
					ref, label := cueAt, "after cue"
					if !calleeEnd.IsZero() {
						ref, label = calleeEnd, "after callee line end (incl. VAD)"
					}
					fmt.Println(at(), "first agent audio,", time.Since(ref).Round(10*time.Millisecond), label)
				}
				samples += len(e.PCM)
				if sink != nil {
					_ = binary.Write(sink, binary.LittleEndian, e.PCM)
				}
			case voice.TurnEnd:
				turns++
				fmt.Println(at(), "turn end")
				if turns == 1 && callee != nil {
					// The callee answers once the agent's turn would have finished playing.
					playLeft := time.Duration(samples)*time.Second/8000 - time.Since(firstAudioAt)
					newSink(2)
					go func(pcm []int16) {
						time.Sleep(max(playLeft, 0) + 800*time.Millisecond)
						stop()
						for i := 0; i < len(pcm); i += 160 {
							_ = sess.SendAudio(pcm[i:min(i+160, len(pcm))])
							time.Sleep(20 * time.Millisecond)
						}
						fmt.Println(at(), "callee line sent")
						tk := time.NewTicker(20 * time.Millisecond)
						defer tk.Stop()
						for range tk.C {
							if sess.SendAudio(make([]int16, 160)) != nil {
								return
							}
						}
					}(callee)
					calleeEnd = time.Now().Add(max(playLeft, 0) + 800*time.Millisecond + time.Duration(len(callee)/8)*time.Millisecond)
					firstAudio = false
				}
			case voice.Transcript:
				fmt.Println(at(), e.Speaker+":", e.Text)
			case voice.Interrupted:
				fmt.Println(at(), "interrupted")
			case voice.Outcome:
				fmt.Println(at(), "outcome:", e.Result, e.Summary)
			case voice.EndCall:
				fmt.Println(at(), "end_call:", e.Reason)
			case voice.Usage:
				if *trace {
					fmt.Println(at(), "usage", map[string]int(e))
				}
			}
		}
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}
