package audio

import "testing"

func TestApologyFrames(t *testing.T) {
	frames, err := ApologyFrames()
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 40 {
		t.Fatalf("got %d packets, want >= 40", len(frames))
	}
	if len(frames)*20 < 1000 {
		t.Fatalf("duration %d ms < 1 s", len(frames)*20)
	}
	dec, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range frames {
		if len(p) < 1 || len(p) > 4087 {
			t.Fatalf("packet %d size %d out of range", i, len(p))
		}
		pcm, err := dec.Decode(p) // 24 kHz-encoded 20 ms packet decodes to 320 samples at 16 kHz
		if err != nil || len(pcm) != UpstreamFrame {
			t.Fatalf("packet %d decode: n=%d err=%v", i, len(pcm), err)
		}
	}
	if ApologyText == "" {
		t.Fatal("empty apology text")
	}
}
