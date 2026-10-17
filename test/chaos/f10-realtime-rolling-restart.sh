#!/usr/bin/env bash
# F10 (spec 13.13.2): see `go run ./test/chaos/cmd/kotoki-chaos list`. Arguments go to kotoki-chaos run:
#   test/chaos/f10-realtime-rolling-restart.sh --env compose --compose-project P --metrics-urls ... --users-file ... --db ...
# The stack comes from `make chaos-local SCEN=F10` (local) or is the staging cluster (--env kube).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
exec go run ./test/chaos/cmd/kotoki-chaos run F10 "$@"
