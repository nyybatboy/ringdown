package gemini

import "math"

// decimator turns 24 kHz PCM into 8 kHz: a windowed-sinc low-pass below the phone band's top,
// then every third sample. It keeps its history and phase across chunks, so chunk edges don't
// click (dialout's linear resampler is stateless per chunk; fine for ElevenLabs at μ-law, not for
// a 3:1 drop).
type decimator struct {
	taps  []float64
	hist  []float64 // the last len(taps)-1 input samples
	phase int       // input samples to skip before the next output
}

const decimFactor = 3

func newDecimator() *decimator {
	const n = 63
	const fc = 3600.0 / 24000.0 // cutoff as a fraction of the input rate
	taps := make([]float64, n)
	var sum float64
	for i := range taps {
		x := float64(i) - float64(n-1)/2
		sinc := 2 * fc
		if x != 0 {
			sinc = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		w := 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(n-1)) // Hamming
		taps[i] = sinc * w
		sum += taps[i]
	}
	for i := range taps {
		taps[i] /= sum
	}
	return &decimator{taps: taps, hist: make([]float64, n-1)}
}

func (d *decimator) process(in []int16) []int16 {
	buf := make([]float64, 0, len(d.hist)+len(in))
	buf = append(buf, d.hist...)
	for _, v := range in {
		buf = append(buf, float64(v))
	}
	n := len(d.taps)
	var out []int16
	i := d.phase
	for ; i < len(in); i += decimFactor {
		// output aligned to input sample i: buf[i .. i+n-1] ends at input sample i
		var acc float64
		w := buf[i : i+n]
		for k, t := range d.taps {
			acc += w[k] * t
		}
		out = append(out, clamp16(acc))
	}
	d.phase = i - len(in)
	copy(d.hist, buf[len(buf)-(n-1):])
	return out
}

func clamp16(v float64) int16 {
	v = math.Round(v)
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}
