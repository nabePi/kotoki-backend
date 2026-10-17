#!/usr/bin/env bash
# F7 (spec 13.13.2): see `go run ./test/chaos/cmd/kotoki-chaos list`. Arguments go to kotoki-chaos run:
#   test/chaos/f07-rabbitmq-node-down.sh --env compose --compose-project P --metrics-urls ... --users-file ... --db ...
# The stack comes from `make chaos-local SCEN=F7` (local) or is the staging cluster (--env kube).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
exec go run ./test/chaos/cmd/kotoki-chaos run F7 "$@"
