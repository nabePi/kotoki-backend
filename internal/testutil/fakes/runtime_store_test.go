package fakes_test

import (
	"testing"

	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

func TestRuntimeStoreContract(t *testing.T) {
	repotest.RuntimeStoreSuite(t, func(*testing.T) rtstate.Store { return fakes.NewRuntimeStore() })
}
