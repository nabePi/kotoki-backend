package bench

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// AudioSource yields the audio of an STT item as PCM16LE mono 16 kHz.
type AudioSource interface {
	PCM16k(it Item) ([]byte, error)
}

// DirAudio reads audio files from the local disk. A `file://` URI or an absolute path is read as is;
// any other URI (a relative path, or `s3://bucket/key` after `aws s3 sync` into Root) is looked up
// under Root: as given, then without its first path segment (the dataset version, e.g. `v1/`), then by
// base name. Nothing outside Root is read through a relative URI. WAV only; other sample rates are
// resampled to 16 kHz.
type DirAudio struct{ Root string }

// PCM16k implements AudioSource.
func (d DirAudio) PCM16k(it Item) ([]byte, error) {
	p, err := d.resolve(it.AudioURI)
	if err != nil {
		return nil, fmt.Errorf("item %s: %w", it.ID, err)
	}
	b, err := os.ReadFile(p) //nolint:gosec // the path is the dataset manifest's audio_uri under the operator's audio root
	if err != nil {
		return nil, fmt.Errorf("item %s: audio: %w", it.ID, err)
	}
	pcm, rate, err := provider.DecodeWAV(b)
	if err != nil {
		return nil, fmt.Errorf("item %s: %w", it.ID, err)
	}
	if rate != provider.STTSampleRate {
		pcm = provider.ResamplePCM16(pcm, rate, provider.STTSampleRate)
	}
	return pcm, nil
}

func (d DirAudio) resolve(uri string) (string, error) {
	if uri == "" {
		return "", errors.New("empty audio_uri")
	}
	var key string
	switch {
	case strings.HasPrefix(uri, "file://"):
		u, err := url.Parse(uri)
		if err != nil || u.Path == "" {
			return "", fmt.Errorf("bad file URI %q", uri)
		}
		return u.Path, nil
	case strings.HasPrefix(uri, "s3://"):
		rest := strings.TrimPrefix(uri, "s3://")
		_, k, ok := strings.Cut(rest, "/")
		if !ok || k == "" {
			return "", fmt.Errorf("bad s3 URI %q", uri)
		}
		key = k
	case filepath.IsAbs(uri):
		return uri, nil
	default:
		key = uri
	}
	if d.Root == "" {
		return "", fmt.Errorf("audio %q needs --audio-root", uri)
	}
	key = path.Clean("/" + filepath.ToSlash(key))[1:] // no ".." out of Root
	if key == "" {
		return "", fmt.Errorf("bad audio path %q", uri)
	}
	cands := []string{key}
	if _, rest, ok := strings.Cut(key, "/"); ok {
		cands = append(cands, rest)
	}
	cands = append(cands, path.Base(key))
	for _, c := range cands {
		p := filepath.Join(d.Root, filepath.FromSlash(c))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("audio %q not found under the audio root", uri)
}
