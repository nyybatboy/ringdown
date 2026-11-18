package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mfbonfigli/dialout/internal/voice"
)

// TestFlushOnClose: a caller who speaks and hangs up before turnComplete still leaves their words
// in the transcript (the first test of a live line, 2026-11-16: the answering side kept only
// its own greeting).
func TestFlushOnClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := context.Background()
		for _, m := range []string{
			`{"serverContent":{"outputTranscription":{"text":"Hello, Sam's phone."}}}`,
			`{"serverContent":{"turnComplete":true}}`,
			`{"serverContent":{"inputTranscription":{"text":"Tell him the line test "}}}`,
			`{"serverContent":{"inputTranscription":{"text":"went through."}}}`,
		} {
			_ = ws.Write(ctx, websocket.MessageText, []byte(m))
		}
		time.Sleep(100 * time.Millisecond)
		ws.Close(websocket.StatusNormalClosure, "bye")
	}))
	defer srv.Close()
	ctx := context.Background()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(ctx, ws)
	var lines []string
	for ev := range s.Events() {
		if tr, ok := ev.(voice.Transcript); ok {
			lines = append(lines, tr.Speaker+": "+tr.Text)
		}
	}
	want := []string{"agent: Hello, Sam's phone.", "callee: Tell him the line test went through."}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("transcript = %q", lines)
	}
}

func TestCleanTranscript(t *testing.T) {
	for in, want := range map[string]string{
		"Thanks for your help! <no speech>{pause}": "Thanks for your help!",
		"  It's 3 {laughs} pm  ":                   "It's 3 pm",
		"x < y and z > w":                          "x < y and z > w",
	} {
		if got := cleanTranscript(in); got != want {
			t.Errorf("cleanTranscript(%q) = %q", in, got)
		}
	}
}
