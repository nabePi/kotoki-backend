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
K8S_ENVS        ?= dev staging prod
K8S_TOOLS       := $(CURDIR)/bin/tools
# base selalu dibangun (assert Task 2-9 membaca out/k8s/base.yaml); overlay yang belum ada dilewati
# kecuali K8S_REQUIRE_OVERLAYS=1 (wajib dinyalakan di CI setelah Task 11).
K8S_REQUIRE_OVERLAYS ?= 0
K8S_BUILT       := base $(foreach e,$(K8S_ENVS),$(if $(wildcard deploy/k8s/overlays/$(e)/kustomization.yaml),$(e)))

.PHONY: env up up-obs down reset migrate-up migrate-down migrate-create seed promote-admin \
        sqlc sqlc-check tidy-check migrations-check modelname-check arch-check assets-apology fixtures fixtures16k \
        test test-cover test-cover-integration test-integration test-e2e test-live e2e-real lint vuln \
        run-api run-realtime run-worker build cli docker-build load \
        k8s-tools k8s-build k8s-validate k8s-assert prom-check k8s-test dashboards dashboards-check \
        load-local verify chaos-local capacity readiness-check pins-check

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

e2e-real:       ## MANUAL GATE: sesi e2e PROVIDER_MODE=real skala kecil (kunci vendor + voice ID nyata, lihat test/e2e/real/run.sh); tanpa kunci exit 2
	test/e2e/real/run.sh

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

# ---- M10: manifest Kubernetes (validasi offline; tidak butuh cluster) ----
k8s-tools:      ## unduh kustomize/kubeconform/promtool/yq/actionlint (versi: deploy/k8s/hack/tools.env) ke bin/tools
	deploy/k8s/hack/install-tools.sh

k8s-build:      ## kustomize build base + tiap overlay -> out/k8s/<nama>.yaml
	@mkdir -p out/k8s
	@echo "kustomize build base"
	@$(K8S_TOOLS)/kustomize build deploy/k8s/base > out/k8s/base.yaml.tmp || { rm -f out/k8s/base.yaml.tmp; exit 1; }
	@mv out/k8s/base.yaml.tmp out/k8s/base.yaml
	@for e in $(K8S_ENVS); do \
	  if [ ! -f deploy/k8s/overlays/$$e/kustomization.yaml ]; then \
	    [ "$(K8S_REQUIRE_OVERLAYS)" != 1 ] || { echo "k8s-build: overlay deploy/k8s/overlays/$$e belum ada"; exit 1; }; \
	    echo "k8s-build: SKIP overlay $$e (belum ada; Task 10/11)"; continue; \
	  fi; \
	  echo "kustomize build overlays/$$e"; \
	  $(K8S_TOOLS)/kustomize build deploy/k8s/overlays/$$e > out/k8s/$$e.yaml.tmp || { rm -f out/k8s/$$e.yaml.tmp; exit 1; }; \
	  mv out/k8s/$$e.yaml.tmp out/k8s/$$e.yaml; \
	done

k8s-validate: k8s-build  ## kubeconform strict (skema default + katalog CRD datreeio)
	deploy/k8s/hack/validate.sh $(K8S_BUILT)

k8s-assert: k8s-build  ## deploy/k8s/assert/*.sh (berbasis yq); tanpa skrip = gagal, bukan lulus
	@set -e; n=0; for s in deploy/k8s/assert/*.sh; do \
	  [ -e "$$s" ] || break; n=$$((n+1)); echo "== $$s"; bash "$$s"; done; \
	  [ "$$n" -gt 0 ] || { echo "k8s-assert: tidak ada skrip di deploy/k8s/assert/"; exit 1; }

prom-check:     ## promtool check rules + test rules (alert, Task 8)
	@[ -f deploy/k8s/base/observability/prometheusrule.yaml ] || { echo "prom-check: prometheusrule.yaml belum ada (Task 8)"; exit 1; }
	@[ -x deploy/k8s/hack/extract-rules.sh ] || { echo "prom-check: deploy/k8s/hack/extract-rules.sh belum ada (Task 8)"; exit 1; }
	deploy/k8s/hack/extract-rules.sh
	$(K8S_TOOLS)/promtool check rules out/rules.yaml
	$(K8S_TOOLS)/promtool test rules deploy/k8s/base/observability/alerts_test.yaml

k8s-test: k8s-build k8s-validate k8s-assert prom-check  ## build + validate + assert + prom-check + test Go render overlay
	$(GO) test -race -count=1 -tags k8s ./deploy/...

DASHBOARD_DIR := deploy/k8s/base/observability/dashboards
dashboards:     ## render 6 dashboard Grafana dari test/tools/dashgen (Task 9)
	@[ -d test/tools/dashgen ] || { echo "dashboards: test/tools/dashgen belum ada (Task 9)"; exit 1; }
	$(GO) run ./test/tools/dashgen -out $(DASHBOARD_DIR)

dashboards-check: dashboards  ## gagal bila JSON di repo berbeda dari hasil render
	@git diff --exit-code -- $(DASHBOARD_DIR) && [ -z "$$(git ls-files --others --exclude-standard -- $(DASHBOARD_DIR))" ] \
	  || { echo "dashboards-check: $(DASHBOARD_DIR) tidak sinkron; jalankan make dashboards dan commit"; exit 1; }

# ---- M10: harness load / verify / chaos / kapasitas (diisi Task 15-22) ----
load-local:     ## G1 lokal terhadap compose (Task 19)
	@[ -x test/load/local/run.sh ] || { echo "load-local: test/load/local/run.sh belum ada (Task 19)"; exit 1; }
	test/load/local/run.sh $(ARGS)

verify:         ## invarian pasca-run §13.12.6; make verify ARGS="--db ..." (Task 18)
	@[ -d test/load/cmd/kotoki-verify ] || { echo "verify: test/load/cmd/kotoki-verify belum ada (Task 18)"; exit 1; }
	$(GO) run ./test/load/cmd/kotoki-verify $(ARGS)

chaos-local:    ## make chaos-local SCEN=F1 ARGS="--vus 20 --time-scale 0.1" (Task 21)
	@[ -d test/chaos/cmd/kotoki-chaos ] || { echo "chaos-local: test/chaos/cmd/kotoki-chaos belum ada (Task 21)"; exit 1; }
	@[ -n "$(SCEN)" ] || { echo "chaos-local: SCEN wajib, contoh: make chaos-local SCEN=F1"; exit 1; }
	CGO_ENABLED=1 $(GO) run ./test/chaos/cmd/kotoki-chaos --scenario $(SCEN) $(ARGS)

capacity:       ## tabel kapasitas §13.11; make capacity ARGS="--ccu 1000" (Task 22)
	@[ -d test/load/capacity ] || { echo "capacity: test/load/capacity belum ada (Task 22)"; exit 1; }
	$(GO) run ./test/load/cmd/kotoki-load capacity $(ARGS)

readiness-check: ## validasi docs/production-readiness.md (Task 23)
	@[ -d test/tools/readinesscheck ] || { echo "readiness-check: test/tools/readinesscheck belum ada (Task 23)"; exit 1; }
	$(GO) run ./test/tools/readinesscheck

pins-check:     ## gagal bila <PIN>/placeholder tertinggal; make pins-check ARGS=--prod (Task 11/23)
	@[ -x deploy/k8s/hack/pins-check.sh ] || { echo "pins-check: deploy/k8s/hack/pins-check.sh belum ada (Task 11)"; exit 1; }
	deploy/k8s/hack/pins-check.sh $(ARGS)
