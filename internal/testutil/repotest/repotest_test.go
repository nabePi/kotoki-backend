package repotest_test

import (
	"testing"

	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

type counter struct{ n int }

func TestEachFreshRepo(t *testing.T) {
	made := 0
	mk := func(t *testing.T) *counter { made++; return &counter{} }
	c := repotest.Case[*counter]{Name: "x", Run: func(t *testing.T, r *counter) {
		if r.n != 0 {
			t.Fatalf("state leaked: %d", r.n)
		}
		r.n++
	}}
	repotest.Each(t, mk, c, c, c)
	if made != 3 {
		t.Fatalf("made=%d", made)
	}
}
