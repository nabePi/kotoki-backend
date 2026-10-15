package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
)

// fundedUser registers a user and grants seconds through the ledger so it has entries to list.
func (e *apiEnv) fundedUser(t *testing.T, email string, seconds int64) (authz string) {
	t.Helper()
	authz = bearerOf(t, e.register(t, email))
	u := e.userByEmail(t, email)
	require.NoError(t, e.bal.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
		acc, err := tx.Balance().LockAccountByUserID(ctx, u.ID)
		if err != nil {
			return err
		}
		_, err = tx.Balance().ApplyMovement(ctx, acc, balance.Movement{
			EntryType: balance.EntryGrant, AvailableDelta: seconds, ReferenceType: balance.RefRegistration,
			IdempotencyKey: "grant:initial:" + u.ID.String(), At: apiT0,
		})
		return err
	}))
	return authz
}

func TestBalanceGolden(t *testing.T) {
	e := newAPI(t)
	authz := e.fundedUser(t, "hana@example.com", 1200)
	rec := e.do(http.MethodGet, "/api/v1/balance", "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "balance", "balance_200.json", rec.Body.Bytes())

	var b struct {
		Allowed []int `json:"allowed_durations"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &b))
	require.Equal(t, []int{300, 600, 900}, b.Allowed)

	empty := bearerOf(t, e.register(t, "empty@example.com"))
	rec = e.do(http.MethodGet, "/api/v1/balance", "", "Authorization", empty)
	require.Contains(t, rec.Body.String(), `"allowed_durations":[]`)
}

func TestBalanceTransactionsUserShape(t *testing.T) {
	e := newAPI(t)
	authz := e.fundedUser(t, "hana@example.com", 1200)
	rec := e.do(http.MethodGet, "/api/v1/balance/transactions", "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())

	var out struct {
		Data       []map[string]any `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Data, 1)
	var keys []string
	for k := range out.Data[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	require.Equal(t, []string{"available_seconds_after", "available_seconds_delta", "created_at", "description", "entry_type",
		"id", "reserved_seconds_after", "reserved_seconds_delta", "session_id"}, keys, "exactly the nine user-facing fields")
	require.Equal(t, "Welcome balance", out.Data[0]["description"])
	require.Nil(t, out.Data[0]["session_id"])
	require.Nil(t, out.NextCursor)

	ok := e.do(http.MethodGet, "/api/v1/balance/transactions?entry_type=GRANT,CONSUME&limit=5", "", "Authorization", authz)
	require.Equal(t, 200, ok.Code)
	bad := e.do(http.MethodGet, "/api/v1/balance/transactions?entry_type=BOGUS", "", "Authorization", authz)
	require.Equal(t, 400, bad.Code)
	require.Equal(t, "VALIDATION_FAILED", errorBody(t, bad)["code"])
	bad = e.do(http.MethodGet, "/api/v1/balance/transactions?cursor=garbage", "", "Authorization", authz)
	require.Equal(t, "INVALID_CURSOR", errorBody(t, bad)["code"])
	bad = e.do(http.MethodGet, "/api/v1/balance/transactions?limit=101", "", "Authorization", authz)
	require.Equal(t, 400, bad.Code)
}

func TestBalanceTransactionsPagination(t *testing.T) {
	e := newAPI(t)
	authz := e.fundedUser(t, "hana@example.com", 600)
	u := e.userByEmail(t, "hana@example.com")
	for i, key := range []string{"grant:k1", "grant:k2"} {
		_ = i
		require.NoError(t, e.bal.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
			acc, err := tx.Balance().LockAccountByUserID(ctx, u.ID)
			if err != nil {
				return err
			}
			_, err = tx.Balance().ApplyMovement(ctx, acc, balance.Movement{EntryType: balance.EntryGrant, AvailableDelta: 60,
				ReferenceType: balance.RefRegistration, IdempotencyKey: key, At: apiT0})
			return err
		}))
	}
	rec := e.do(http.MethodGet, "/api/v1/balance/transactions?limit=2", "", "Authorization", authz)
	var p1 struct {
		Data       []map[string]any `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p1))
	require.Len(t, p1.Data, 2)
	require.NotNil(t, p1.NextCursor)

	rec = e.do(http.MethodGet, "/api/v1/balance/transactions?limit=2&cursor="+*p1.NextCursor, "", "Authorization", authz)
	var p2 struct {
		Data       []map[string]any `json:"data"`
		NextCursor *string          `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p2))
	require.Len(t, p2.Data, 1)
	require.Nil(t, p2.NextCursor)
}
