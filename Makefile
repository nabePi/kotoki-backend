SHELL := /bin/bash
-include .env
export

GO              ?= go
# Container engine: docker when present, otherwise rootless podman. testcontainers
# talks to the podman API socket (start it with: systemctl --user start podman.socket).
ifeq ($(shell command -v docker 2>/dev/null),)
CONTAINER_ENGINE ?= podman
export DOCKER_HOST ?= unix://$(XDG_RUNTIME_DIR)/podman/podman.sock
else
CONTAINER_ENGINE ?= docker
endif
COMPOSE ?= $(CONTAINER_ENGINE) compose
MIGRATE_IMAGE   := docker.io/migrate/migrate:v4.18.3
COMPOSE_NETWORK := kotoki_default
MIGRATE_DB_URL  := postgres://kotoki:kotoki@postgres:5432/kotoki?sslmode=disable
GOLANGCI_LINT_VERSION := v2.14.0
VERSION         ?= $(shell git describe --tags --always --dirty)
GIT_COMMIT      ?= $(shell git rev-parse --short HEAD)
N               ?= 1

.PHONY: env up up-obs down reset migrate-up migrate-down migrate-create seed promote-admin \
        sqlc sqlc-check tidy-check migrations-check modelname-check arch-check assets-apology fixtures fixtures16k \
        test test-cover test-cover-integration test-integration test-e2e test-live lint vuln \
        run-api run-realtime run-worker build cli docker-build load

env:            ## salin .env.example → .env (tidak menimpa)
	@test -f .env || cp .env.example .env

up:             ## jalankan dependency lokal
	$(COMPOSE) up -d --wait

up-obs:         ## dependency + otel-collector + grafana/otel-lgtm (Grafana http://localhost:3000)
	$(COMPOSE) --profile observability up -d --wait

down:
	$(COMPOSE) --profile observability down

reset:          ## hapus volume (data lokal hilang)
	$(COMPOSE) --profile observability down -v

migrate-up:
	$(CONTAINER_ENGINE) run --rm --network $(COMPOSE_NETWORK) -v $(CURDIR)/migrations:/migrations \
	  $(MIGRATE_IMAGE) -path=/migrations -database "$(MIGRATE_DB_URL)" up

migrate-down:   ## turun N langkah (default 1): make migrate-down N=3
	$(CONTAINER_ENGINE) run --rm --network $(COMPOSE_NETWORK) -v $(CURDIR)/migrations:/migrations \
	  $(MIGRATE_IMAGE) -path=/migrations -database "$(MIGRATE_DB_URL)" down $(N)

migrate-create: ## make migrate-create NAME=add_x
	$(CONTAINER_ENGINE) run --rm -v $(CURDIR)/migrations:/migrations -u $$(id -u):$$(id -g) \
	  $(MIGRATE_IMAGE) create -ext sql -dir /migrations -seq -digits 6 $(NAME)

seed:           ## jalankan seed/*.sql lalu seed/env/$KOTOKI_ENV/*.sql (default local, opsional); idempoten
	@for f in seed/*.sql seed/env/$${KOTOKI_ENV:-local}/*.sql; do [ -f "$$f" ] || continue; echo "seed $$f"; \
	  $(COMPOSE) exec -T postgres psql -U kotoki -d kotoki -v ON_ERROR_STOP=1 < $$f || exit 1; done

promote-admin:  ## make promote-admin EMAIL=dev@example.test
	$(COMPOSE) exec -T postgres psql -U kotoki -d kotoki -v ON_ERROR_STOP=1 \
	  -c "UPDATE users SET role='ADMIN', updated_at=now() WHERE email='$(EMAIL)'"

sqlc:
	$(GO) tool sqlc generate

sqlc-check: sqlc
	git diff --exit-code -- internal/postgres/sqlcgen

tidy-check:
	$(GO) mod tidy && git diff --exit-code -- go.mod go.sum

migrations-check:
	$(GO) run ./test/tools/migcheck -dir migrations -base origin/main

modelname-check:
	$(GO) run ./test/tools/modelcheck ./internal ./cmd

arch-check:     ## §2.2 aturan 5: api & worker tanpa cgo (depguard menangani impor langsung); aturan impor provider/prompt
	$(GO) test ./internal/provider ./internal/prompt -run 'TestImportRules' -count=1
	@pkgs=""; for p in api worker; do [ -d cmd/$$p ] && pkgs="$$pkgs ./cmd/$$p"; done; \
	if [ -z "$$pkgs" ]; then echo 'arch-check: cmd/api dan cmd/worker belum ada, dilewati (dibuat di Task 23)'; exit 0; fi; \
	CGO_ENABLED=0 $(GO) build -o /dev/null $$pkgs || exit 1; \
	if $(GO) list -deps $$pkgs | grep -E 'internal/conversation/audio|hraban/opus|go-webrtcvad'; then \
	  echo 'cmd/api atau cmd/worker bergantung pada paket cgo (§2.2 aturan 5)'; exit 1; fi

assets-apology: ## regenerate audio/assets/apology_ja.opus (mode fake; M9 menambah -tts)
	$(GO) run ./test/tools/genapology -o internal/conversation/audio/assets/apology_ja.opus

fixtures:       ## regenerate testdata/audio/synth (fixture sintetis §14.5, deterministik)
	$(GO) run ./test/tools/genaudio -out testdata/audio/synth

fixtures16k:    ## salinan 16 kHz + .txt untuk FAKE_STT_FIXTURE_DIR (out/fixtures16k)
	$(GO) run ./test/tools/genaudio -to16k out/fixtures16k -src testdata/audio/synth

test:           ## unit + contract + component, race
	$(GO) test -race -short -count=1 ./...

test-cover:
	$(GO) test -race -short -count=1 -coverprofile=coverage.out ./...
	$(GO) run ./test/tools/covercheck -profile coverage.out -thresholds test/tools/covercheck/thresholds.txt

test-cover-integration: ## cakupan adapter (postgres, redisstore, amqp) dari seluruh run integration, spec §14.8
	@pkgs=""; for p in postgres redisstore amqp; do [ -d internal/$$p ] && pkgs="$$pkgs,./internal/$$p/..."; done; \
	$(GO) test -race -count=1 -p 1 -tags=integration -timeout 25m -coverpkg=$${pkgs#,} -coverprofile=coverage.integration.out ./...
	$(GO) run ./test/tools/covercheck -profile coverage.integration.out -thresholds test/tools/covercheck/thresholds.integration.txt

test-integration:
	$(GO) test -race -count=1 -p 1 -tags=integration -timeout 20m ./...

test-e2e:
	$(GO) test -race -count=1 -tags=e2e -timeout 30m ./test/e2e/...

test-live:      ## butuh kunci provider di .env
	$(GO) test -race -count=1 -tags=live -timeout 10m ./internal/provider/...

lint:           ## golangci-lint $(GOLANGCI_LINT_VERSION); pasang: GOBIN=$$PWD/bin/tools go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	PATH="$(CURDIR)/bin/tools:$$PATH" golangci-lint run ./...

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

run-api:
	$(GO) run ./cmd/api

run-realtime: fixtures16k  ## cgo (opus, webrtcvad); PROVIDER_MODE=fake membaca FAKE_STT_FIXTURE_DIR=out/fixtures16k
	CGO_ENABLED=1 $(GO) run ./cmd/realtime

WORKER_HANDLERS ?= outbox,sweeper,cleanup
run-worker:      ## WORKER_HTTP_ADDR default Bab 9 (:8081) bentrok dengan realtime di host → :9092; handler evaluation/memory/learner/summary menyusul (M7/M8)
	WORKER_HTTP_ADDR=:9092 $(GO) run ./cmd/worker --handlers=$(WORKER_HANDLERS)

build:
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/ ./cmd/api ./cmd/worker
	CGO_ENABLED=1 $(GO) build -trimpath -o bin/ ./cmd/realtime $$([ -d cmd/kotoki-cli ] && echo ./cmd/kotoki-cli)

cli:
	CGO_ENABLED=1 $(GO) build -o bin/kotoki-cli ./cmd/kotoki-cli

docker-build:
	for b in api realtime worker migrate; do \
	  $(CONTAINER_ENGINE) build -f deploy/docker/$$b.Dockerfile --build-arg VERSION=$(VERSION) \
	    --build-arg GIT_COMMIT=$(GIT_COMMIT) -t kotoki/$$b:$(VERSION) . || exit 1; done

load:           ## make load ARGS="--vus 20 --hold 5m"
	CGO_ENABLED=1 $(GO) run ./test/load/cmd/kotoki-load $(ARGS)
