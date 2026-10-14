//go:build integration || e2e

package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func merge(base, o Row) Row {
	for k, v := range o {
		base[k] = v
	}
	return base
}

// UserRow returns a minimal valid users row with a unique email.
func UserRow(o Row) Row {
	id := uuid.Must(uuid.NewV7())
	return merge(Row{
		"id":                id,
		"email":             id.String() + "@example.test",
		"full_name":         "Test User",
		"password_hash":     "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"terms_version":     "2026-01",
		"terms_accepted_at": time.Now().UTC(),
	}, o)
}

// SeedUser inserts a user and returns its id.
func SeedUser(t testing.TB, db DBTX, o Row) uuid.UUID {
	t.Helper()
	r := UserRow(o)
	MustInsert(t, context.Background(), db, "users", r)
	return r["id"].(uuid.UUID)
}

// BalanceAccountRow returns a balance_accounts row for userID.
func BalanceAccountRow(userID uuid.UUID, o Row) Row {
	return merge(Row{
		"id":      uuid.Must(uuid.NewV7()),
		"user_id": userID,
	}, o)
}
