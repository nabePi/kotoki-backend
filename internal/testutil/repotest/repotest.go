// Package repotest provides a skeleton for running a repository contract
// suite against any implementation.
package repotest

import "testing"

// Case is one named contract check against a repository.
type Case[T any] struct {
	Name string
	Run  func(t *testing.T, repo T)
}

// Each runs every case as a subtest with a fresh repository.
func Each[T any](t *testing.T, newRepo func(t *testing.T) T, cases ...Case[T]) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			c.Run(t, newRepo(t))
		})
	}
}
