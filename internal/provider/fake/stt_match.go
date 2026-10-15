package fake

import "math"

const sttFrameMs = 20

// frameRMS returns the linear RMS of each full 20 ms frame (640 bytes at 16 kHz).
func frameRMS(pcm []byte, frameBytes int) []float64 {
	out := make([]float64, 0, len(pcm)/frameBytes)
	for off := 0; off+frameBytes <= len(pcm); off += frameBytes {
		out = append(out, rmsLinear(pcm[off:off+frameBytes]))
	}
	return out
}

func rmsLinear(frame []byte) float64 {
	n := len(frame) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(int16(uint16(frame[i*2]) | uint16(frame[i*2+1])<<8)) //nolint:gosec // two's complement reinterpretation of a PCM sample
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}

func rmsDBFS(rms float64) float64 {
	if rms <= 0 {
		return -120
	}
	return math.Max(20*math.Log10(rms/32768), -120)
}

// normalize scales an envelope by its maximum (all zeros stay zeros).
func normalize(env []float64) []float64 {
	var peak float64
	for _, v := range env {
		peak = math.Max(peak, v)
	}
	out := make([]float64, len(env))
	if peak == 0 {
		return out
	}
	for i, v := range env {
		out[i] = v / peak
	}
	return out
}

// pearson is the correlation of two equal-length vectors (mean removed); constant → 0.
func pearson(a, b []float64) float64 {
	n := len(a)
	if n == 0 || n != len(b) {
		return 0
	}
	var ma, mb float64
	for i := range a {
		ma += a[i]
		mb += b[i]
	}
	ma /= float64(n)
	mb /= float64(n)
	var num, da, db float64
	for i := range a {
		x, y := a[i]-ma, b[i]-mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da == 0 || db == 0 {
		return 0
	}
	return num / math.Sqrt(da*db)
}

// matchScore is the best normalised cross-correlation of two envelopes over shifts of
// ±maxShift frames. The correlation is scaled by overlap/longer-length so a short input
// cannot match a long fixture on a lucky stretch.
func matchScore(in, fixture []float64, maxShift int) float64 {
	in, fixture = normalize(in), normalize(fixture)
	longer := max(len(in), len(fixture))
	if longer == 0 {
		return 0
	}
	best := 0.0
	for shift := -maxShift; shift <= maxShift; shift++ {
		// pair in[i] with fixture[i+shift]
		lo := max(0, -shift)
		hi := min(len(in), len(fixture)-shift)
		if hi-lo < 2 {
			continue
		}
		score := pearson(in[lo:hi], fixture[lo+shift:hi+shift]) * float64(hi-lo) / float64(longer)
		best = math.Max(best, score)
	}
	return best
}
