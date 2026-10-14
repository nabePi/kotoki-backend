package config

import "time"

// Content holds the settings of the content and session read paths (spec 6.5, 5.9.4).
type Content struct {
	// ContentCacheTTLSeconds is how long a pod serves cached "live" content before reading it again
	// (also the longest another pod needs to notice a publish, spec 6.5).
	ContentCacheTTLSeconds int `env:"CONTENT_CACHE_TTL_SECONDS"`
	// RetentionTranscriptDays is the transcript retention counted from sessions.ended_at (D-099);
	// GET /sessions/{id}/turns answers 410 after it.
	RetentionTranscriptDays int `env:"RETENTION_TRANSCRIPT_DAYS"`
}

func defaultContent() Content {
	return Content{ContentCacheTTLSeconds: 30, RetentionTranscriptDays: 180}
}

// CacheTTL is ContentCacheTTLSeconds as a duration.
func (c Content) CacheTTL() time.Duration {
	return time.Duration(c.ContentCacheTTLSeconds) * time.Second
}

func (c Content) validate(v *validator) {
	if c.ContentCacheTTLSeconds < 1 || c.ContentCacheTTLSeconds > 3600 {
		v.fail("CONTENT_CACHE_TTL_SECONDS", "must be within 1–3600, got %d", c.ContentCacheTTLSeconds)
	}
	if c.RetentionTranscriptDays < 1 || c.RetentionTranscriptDays > 3650 {
		v.fail("RETENTION_TRANSCRIPT_DAYS", "must be within 1–3650, got %d", c.RetentionTranscriptDays)
	}
}
