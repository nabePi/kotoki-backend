//go:build !windows

package main

import (
	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
	"github.com/nabePi/kotoki-backend/test/load/server"
)

func promAPI(base string) scenario.Querier { return server.PromAPI{BaseURL: base} }
