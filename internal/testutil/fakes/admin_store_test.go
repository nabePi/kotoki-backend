package fakes_test

import (
	"testing"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

func TestAdminStoreAuditContract(t *testing.T) {
	repotest.AuditRepoSuite(t, func(t *testing.T) admin.Store {
		bal := fakes.NewBalanceStore()
		return fakes.NewAdminStore(fakes.NewAuthStore(bal, idgen.NewSequence(t)), bal)
	})
}
