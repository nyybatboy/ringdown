package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mfbonfigli/dialout/internal/call"
	"github.com/mfbonfigli/dialout/internal/twilio"
	"github.com/mfbonfigli/dialout/internal/voice"
)

const inboundConfig = testConfig + `
tunnel: {hostname: calls.example.com, token: x}
inbound:
  enabled: true
  objective: Take a message for Marco.
`

// post sends a signed (or forged) incoming-call webhook.
func post(t *testing.T, h *harness, form url.Values, signed bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, VoicePath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sig := "forged"
	if signed {
		sig = twilio.Signature("t", "https://calls.example.com"+VoicePath, form)
	}
	req.Header.Set("X-Twilio-Signature", sig)
	w := httptest.NewRecorder()
	h.s.VoiceHandler().ServeHTTP(w, req)
	return w
}

func callForm(sid, from string) url.Values {
	return url.Values{"CallSid": {sid}, "AccountSid": {"AC1"}, "From": {from}, "To": {"+353850000000"}}
}

func TestInbound(t *testing.T) {
	h := newHarnessWith(t, inboundConfig)
	h.s.App.Streams = &call.Streams{}

	if w := post(t, h, callForm("CA1", "+393330000000"), false); w.Code != http.StatusNotFound {
		t.Fatalf("forged webhook: %d %s", w.Code, w.Body)
	}
	w := post(t, h, callForm("CA1", "+393330000000"), true)
	if !strings.Contains(w.Body.String(), `<Stream url="wss://calls.example.com/twilio/stream/`) {
		t.Fatalf("answer = %s", w.Body)
	}
	// One inbound call at a time; an outbound call may still be placed beside it.
	if w := post(t, h, callForm("CA2", "+15550001111"), true); !strings.Contains(w.Body.String(), "<Reject") {
		t.Errorf("second inbound call: %s", w.Body)
	}
	var placed placeCallOut
	h.tool("place_call", map[string]any{"to": "Pizzeria", "objective": "x"}, &placed, false)

	h.mu.Lock()
	p := h.params[0]
	h.mu.Unlock()
	if p.Inbound == nil || p.Inbound.TwilioSID != "CA1" || !p.Brief.Inbound || p.Brief.Callee != "" ||
		!strings.Contains(p.Brief.Context, "caller ID can be faked") || p.Label != "Marco (mobile) (caller ID, unverified)" ||
		p.Brief.Objective != "Take a message for Marco." || p.MaxDuration != 120*time.Minute {
		t.Errorf("inbound params = %+v", p)
	}

	close(h.release)
	time.Sleep(200 * time.Millisecond)
	var inbox listInboxOut
	h.tool("list_inbox", map[string]any{}, &inbox, false)
	if !inbox.Enabled || len(inbox.Calls) != 1 || inbox.Calls[0].Direction != "inbound" || inbox.Calls[0].From != "+393330000000" ||
		inbox.Calls[0].Transcript != nil || inbox.Calls[0].Status != "completed" {
		t.Errorf("inbox = %+v", inbox)
	}
}

func TestInboundOff(t *testing.T) {
	h := newHarnessWith(t, testConfig+"tunnel: {hostname: calls.example.com, token: x}\n")
	h.s.App.Streams = &call.Streams{}
	if w := post(t, h, callForm("CA1", "+15550001111"), true); !strings.Contains(w.Body.String(), "<Reject") {
		t.Errorf("inbound off: %s", w.Body)
	}
}

// With inbound off, the install answers only its own test call: from its own number, once,
// within two minutes of placing it.
func TestSelfTestWithInboundOff(t *testing.T) {
	h := newHarnessWith(t, testConfig+"tunnel: {hostname: calls.example.com, token: x}\n")
	h.s.App.Streams = &call.Streams{}
	if w := post(t, h, callForm("CA1", "+353850000000"), true); !strings.Contains(w.Body.String(), "<Reject") {
		t.Fatalf("own number, not armed: %s", w.Body)
	}
	h.s.mu.Lock()
	h.s.selfTestUntil = time.Now().Add(time.Minute)
	h.s.mu.Unlock()
	if w := post(t, h, callForm("CA2", "+15550001111"), true); !strings.Contains(w.Body.String(), "<Reject") {
		t.Fatalf("other number while armed: %s", w.Body)
	}
	if w := post(t, h, callForm("CA3", "+353 85 000 0000"), true); !strings.Contains(w.Body.String(), "<Stream") {
		t.Fatalf("own number, armed: %s", w.Body)
	}
	h.s.mu.Lock()
	armed := !h.s.selfTestUntil.IsZero()
	h.s.mu.Unlock()
	if armed {
		t.Error("the self test stays armed after its call")
	}
}

func TestSpendLimit(t *testing.T) {
	h := newHarnessWith(t, strings.Replace(testConfig, "limits: {per_hour: 2}", "limits: {per_hour: 2, spend_per_month_usd: 0.01}", 1)+
		"engine: {name: gemini, gemini: {api_key: k}}\n")
	var placed placeCallOut
	h.tool("place_call", map[string]any{"to": "Pizzeria", "objective": "x"}, &placed, false)
	close(h.release)
	var rec Record
	h.tool("get_call", map[string]any{"call_id": placed.CallID, "wait_seconds": 5}, &rec, false)
	// 42 s: one carrier minute at $0.014 plus 0.7 engine minutes at $0.017.
	if rec.CostUSD != 0.0259 || rec.Engine != "gemini" {
		t.Fatalf("record = %+v", rec)
	}
	if msg := h.tool("place_call", map[string]any{"to": "Pizzeria", "objective": "x"}, nil, true); !strings.Contains(msg, "spend_limit") {
		t.Errorf("over the limit: %s", msg)
	}
	var nums listNumbersOut
	h.tool("list_numbers", map[string]any{}, &nums, false)
	if nums.Spend != "$0.03 of $0.01 this month" {
		t.Errorf("spend = %q", nums.Spend)
	}
}

func TestCost(t *testing.T) {
	if c := cost(0, 0.014, 0.08); c != 0 {
		t.Errorf("unanswered = %v", c)
	}
	if c := cost(61*time.Second, 0.014, 0.08); c != 0.1093 {
		t.Errorf("61 s = %v", c)
	}
	loc, _ := time.LoadLocation("America/New_York")
	if m := monthStart(time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC), loc); m.Month() != 11 || m.Day() != 1 {
		t.Errorf("month start = %v", m)
	}
}

func TestRequestID(t *testing.T) {
	h := newHarness(t)
	var first, again placeCallOut
	h.tool("place_call", map[string]any{"to": "Pizzeria", "objective": "x", "request_id": "r1"}, &first, false)
	h.tool("place_call", map[string]any{"to": "Pizzeria", "objective": "x", "request_id": "r1"}, &again, false)
	if again.CallID != first.CallID {
		t.Errorf("retry placed another call: %s then %s", first.CallID, again.CallID)
	}
	close(h.release)
}

func TestDoNotCall(t *testing.T) {
	h := newHarness(t)
	h.s.Index.Add(Record{CallID: "x", Direction: "outbound", To: "+390212345678", Created: time.Now(), Status: "completed",
		Outcome: &voice.Outcome{Result: "do_not_call"}})
	if msg := h.tool("place_call", map[string]any{"to": "Pizzeria", "objective": "x"}, nil, true); !strings.Contains(msg, "do_not_call") {
		t.Errorf("do-not-call number: %s", msg)
	}
}
