package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient/audio"
)

// Common is what every probe needs: a user of the load, the endpoints and the speech fixtures.
type Common struct {
	APIURL, WSURL, Origin string
	Email, Password       string
	FixtureDir            string // directory with manifest.json and the WAVs (or its synth/ subdirectory)
	ClientIP              string // X-Forwarded-For of the probe (local runs behind TRUSTED_PROXY_CIDRS)
	// Out receives one line per result (default: dropped).
	Out func(format string, args ...any)
}

func (c Common) logf(format string, args ...any) {
	if c.Out != nil {
		c.Out(format, args...)
	}
}

// open logs in and reserves a session of the given length from the first scenario and character.
func (c Common) open(ctx context.Context, durationSeconds int) (*kotokiclient.Client, kotokiclient.Session, error) {
	var opts []kotokiclient.Option
	if c.ClientIP != "" {
		opts = append(opts, kotokiclient.WithClientIP(c.ClientIP))
	}
	cl := kotokiclient.New(c.APIURL, opts...)
	if err := cl.Login(ctx, c.Email, c.Password); err != nil {
		return nil, kotokiclient.Session{}, fmt.Errorf("probe login: %w", err)
	}
	cat, err := cl.Catalog(ctx)
	if err != nil {
		return nil, kotokiclient.Session{}, fmt.Errorf("probe catalog: %w", err)
	}
	if len(cat.Scenarios) == 0 || len(cat.Characters) == 0 {
		return nil, kotokiclient.Session{}, errors.New("probe: the catalog has no scenario or character")
	}
	sess, err := cl.CreateSession(ctx, kotokiclient.CreateSessionRequest{
		TopicID: cat.Scenarios[0].TopicID, ScenarioID: cat.Scenarios[0].ID, CharacterID: cat.Characters[0].ID,
		DurationSeconds: durationSeconds, ConversationStartMode: "USER_FIRST",
	})
	if err != nil {
		return nil, kotokiclient.Session{}, fmt.Errorf("probe create session: %w", err)
	}
	return cl, sess, nil
}

// speech loads the named fixture of the manifest (the first one that is not the barge-in when name is
// empty) as pre-encoded Opus packets.
func (c Common) speech(name string) ([][]byte, error) {
	for _, dir := range []string{c.FixtureDir, filepath.Join(c.FixtureDir, "synth")} {
		raw, err := os.ReadFile(filepath.Join(dir, "manifest.json")) //nolint:gosec // G304: the operator chooses the fixture directory
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var m struct {
			Fixtures []struct {
				Name string `json:"name"`
			} `json:"fixtures"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("probe: %s/manifest.json: %w", dir, err)
		}
		for _, f := range m.Fixtures {
			if (name == "" && f.Name == "ja_barge_in") || (name != "" && f.Name != name) {
				continue
			}
			pcm, err := audio.ReadWAV(filepath.Join(dir, f.Name+".wav"))
			if err != nil {
				return nil, fmt.Errorf("probe: fixture %s: %w", f.Name, err)
			}
			return audio.PreEncode(pcm)
		}
	}
	return nil, fmt.Errorf("probe: no speech fixture %q under %s", name, c.FixtureDir)
}
