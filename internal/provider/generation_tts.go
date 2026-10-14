package provider

import "context"

// ttsOut is a router-wrapped TTS stream of a generation (Task 16).
type ttsOut struct{}

// Synthesize is implemented in Task 16.
func (g *generation) Synthesize(_ context.Context, _ TTSRequest) (TTSStream, error) {
	return nil, NewError(g.voice.Primary.Provider, CapTTS, KindInvalidRequest, "generation TTS is not implemented yet")
}
