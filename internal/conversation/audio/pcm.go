package audio

import "encoding/binary"

// Int16ToBytes appends s as little-endian bytes to dst[:0] and returns it.
func Int16ToBytes(s []int16, dst []byte) []byte {
	dst = dst[:0]
	for _, v := range s {
		dst = binary.LittleEndian.AppendUint16(dst, uint16(v)) //nolint:gosec // bit reinterpretation of int16
	}
	return dst
}

// BytesToInt16 decodes little-endian PCM16; a trailing odd byte is ignored.
func BytesToInt16(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[2*i:])) //nolint:gosec // bit reinterpretation of uint16
	}
	return out
}

// Decimate48to16 downsamples by 3, averaging each group of 3 samples.
func Decimate48to16(s []int16) []int16 {
	out := make([]int16, len(s)/3)
	for i := range out {
		out[i] = int16((int(s[3*i]) + int(s[3*i+1]) + int(s[3*i+2])) / 3) //nolint:gosec // mean of three int16 fits int16
	}
	return out
}

// Resample converts s from one rate to another by linear interpolation.
func Resample(s []int16, from, to int) []int16 {
	if from == to || len(s) == 0 || from <= 0 || to <= 0 {
		return append([]int16(nil), s...)
	}
	n := len(s) * to / from
	out := make([]int16, n)
	for i := range out {
		pos := float64(i) * float64(from) / float64(to)
		j := int(pos)
		frac := pos - float64(j)
		a := float64(s[j])
		b := a
		if j+1 < len(s) {
			b = float64(s[j+1])
		}
		out[i] = int16(a + (b-a)*frac)
	}
	return out
}
