package cartesia

import "github.com/nabePi/kotoki-backend/internal/provider"

// STTEndpoint exposes the handshake URL (raw query) to the external tests.
func STTEndpoint(s *STT, cfg provider.STTSessionConfig) string { return s.endpoint(cfg) }
