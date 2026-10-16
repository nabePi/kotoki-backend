package audio

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/hraban/opus.v2"
)

func TestPlaybackDecoderRoundTrip(t *testing.T) {
	enc, err := NewEncoder(PlaybackRate, 32000, 5)
	require.NoError(t, err)
	dec, err := NewPlaybackDecoder()
	require.NoError(t, err)
	var in, out []int16
	for i := 0; i < 50; i++ {
		blk := sine(PlaybackRate, PlaybackFrame, i*PlaybackFrame)
		pkt, err := enc.Encode(blk)
		require.NoError(t, err)
		pcm, err := dec.Decode(pkt)
		require.NoError(t, err)
		require.Len(t, pcm, PlaybackFrame)
		if i >= 5 {
			in = append(in, blk...)
			out = append(out, pcm...)
		}
	}
	require.GreaterOrEqual(t, rms(out), 0.3*rms(in))
}

func TestPlaybackDecoderRejectsBadFrames(t *testing.T) {
	dec, err := NewPlaybackDecoder()
	require.NoError(t, err)
	_, err = dec.Decode([]byte{0xff, 0xff, 0xff})
	require.ErrorIs(t, err, ErrBadFrame)

	// A 40 ms packet decodes fine but is not a 20 ms frame.
	e, err := opus.NewEncoder(PlaybackRate, 1, opus.AppVoIP)
	require.NoError(t, err)
	buf := make([]byte, 4000)
	n, err := e.Encode(sine(PlaybackRate, 2*PlaybackFrame, 0), buf)
	require.NoError(t, err)
	_, err = dec.Decode(buf[:n])
	require.ErrorIs(t, err, ErrBadFrame)
}
