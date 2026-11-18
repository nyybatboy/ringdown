package gemini

import (
	"math"
	"testing"
)

// A 1 kHz tone survives at 8 kHz with its level; a 6 kHz tone (above the phone band) is cut;
// chunking doesn't change the output.
func TestDecimator(t *testing.T) {
	tone := func(f float64, n int) []int16 {
		s := make([]int16, n)
		for i := range s {
			s[i] = int16(10000 * math.Sin(2*math.Pi*f*float64(i)/24000))
		}
		return s
	}
	rms := func(s []int16) float64 {
		var a float64
		for _, v := range s[100:] {
			a += float64(v) * float64(v)
		}
		return math.Sqrt(a / float64(len(s)-100))
	}
	whole := newDecimator().process(tone(1000, 24000))
	if len(whole) != 8000 {
		t.Fatalf("len %d, want 8000", len(whole))
	}
	if r := rms(whole); r < 6500 || r > 7500 {
		t.Fatalf("1 kHz rms %.0f, want ~7071", r)
	}
	if r := rms(newDecimator().process(tone(6000, 24000))); r > 300 {
		t.Fatalf("6 kHz rms %.0f, want cut", r)
	}
	d := newDecimator()
	in := tone(1000, 24000)
	var chunked []int16
	for i := 0; i < len(in); i += 961 { // odd chunk size: phase must carry
		chunked = append(chunked, d.process(in[i:min(i+961, len(in))])...)
	}
	if len(chunked) != len(whole) {
		t.Fatalf("chunked len %d != %d", len(chunked), len(whole))
	}
	for i := range whole {
		if chunked[i] != whole[i] {
			t.Fatalf("sample %d differs: %d vs %d", i, chunked[i], whole[i])
		}
	}
}
