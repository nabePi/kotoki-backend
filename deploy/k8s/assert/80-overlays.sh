#!/usr/bin/env bash
# Overlay staging dan prod (spec §13.2.13, §13.2.17, §13.8.4, §13.10.8; M10 Task 11). Review Focus #5:
# overlay prod tidak boleh bisa boot dengan konfigurasi terlarang. Pemeriksaan `Validate()` sebenarnya ada
# di deploy/k8s/config_test.go (tag k8s); skrip ini memeriksa isi manifest: tidak ada fake, tidak ada
# secret literal, origin https, nilai domain persis, image digest di prod, lima ExternalSecret dengan kunci
# per §13.8.4, RabbitmqCluster merujuk docs/ops/rabbitmq.md, scaler, recording rule infra dan pins-check.
# Pemakaian: 80-overlays.sh [target...]   (default: staging prod)
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../hack/lib.sh
source "$HERE/../hack/lib.sh"

targets=("$@"); [ "${#targets[@]}" -gt 0 ] || targets=(staging prod)
POD='.spec.template.spec'
C="$POD.containers[0]"

# _none <overlay> '<yq-expr yang mencetak pelanggaran>' <label>: lulus bila tidak ada keluaran.
_none() {
  local ov="$1" expr="$2" label="$3" j out
  CHECKS=$((CHECKS + 1))
  j="$(render "$ov")" || { _fail "$ov $label: render gagal"; return 1; }
  out="$("$YQ" -r "$expr" "$j")" || { _fail "$ov $label: yq gagal"; return 1; }
  if [ -n "$out" ]; then _fail "$ov $label: $(tr '\n' ' ' <<<"$out")"; return 1; fi
  _ok "$ov $label"
}

for ov in "${targets[@]}"; do
  case "$ov" in
    staging) APP=https://app.staging.kotoki.id; RT=realtime.staging.kotoki.id; MAILF='Kotoki <no-reply@staging.kotoki.id>' ;;
    prod)    APP=https://app.kotoki.id;         RT=realtime.kotoki.id;         MAILF='Kotoki <no-reply@kotoki.id>' ;;
    *) _fail "$ov: target tidak dikenal (staging|prod)"; continue ;;
  esac

  # ---- invarian lintas-komponen dan area lain (sama dengan base; hanya nilai per overlay yang berbeda)
  for s in 00-common 10-api 20-realtime 30-workers 40-jobs 50-edge 60-observability; do
    CHECKS=$((CHECKS + 1))
    if bash "$HERE/$s.sh" "$ov" >/dev/null; then _ok "$ov $s.sh"; else _fail "$ov $s.sh gagal (jalankan: bash deploy/k8s/assert/$s.sh $ov)"; fi
  done

  # ---- TestProdHasNoFake: tidak ada mode fake, knob fake, atau konfigurasi dev
  expect "$ov" ConfigMap kotoki-common-config '.data.APP_ENV' "$ov"
  expect "$ov" ConfigMap kotoki-provider-config '.data.PROVIDER_MODE' real
  _none "$ov" '.[] | select(.kind == "ConfigMap") | .metadata.name as $n | (.data // {}) | to_entries | .[]
    | select(.key | test("^FAKE_")) | $n + "/" + .key' 'tanpa knob FAKE_*'
  _none "$ov" '.[] | select(.kind == "ConfigMap") | .metadata.name as $n | (.data // {}) | to_entries | .[]
    | select(.key | test("^PROVIDER_MODE")) | select(.value == "fake") | $n + "/" + .key' 'tanpa PROVIDER_MODE*=fake'
  _none "$ov" '.[] | select(.kind == "Deployment") | .metadata.name as $n | .spec.template.spec.containers[0].env[]
    | select(.name | test("^(PROVIDER_MODE|FAKE_)")) | $n + "/" + .name' 'tanpa PROVIDER_MODE/FAKE_* di env Deployment'
  _none "$ov" '.[] | select(.kind == "ConfigMap") | .metadata.name as $n | (.data // {}) | to_entries | .[]
    | select(.key == "LOG_TRANSCRIPTS") | select(.value != "false") | $n + "/" + .key' 'tanpa LOG_TRANSCRIPTS=true'
  _none "$ov" '.[] | select(.kind == "ConfigMap") | .metadata.name as $n | (.data // {}) | to_entries | .[]
    | select(.key == "WS_ALLOW_EMPTY_ORIGIN") | select(.value != "false") | $n + "/" + .key' 'tanpa WS_ALLOW_EMPTY_ORIGIN=true'
  _none "$ov" '.[] | select(.kind == "ConfigMap") | .metadata.name as $n | (.data // {}) | to_entries | .[]
    | select(.key == "MAIL_MODE") | select(.value != "smtp") | $n + "/" + .key' 'MAIL_MODE selalu smtp'
  _none "$ov" '.[] | select(.kind == "ConfigMap") | .metadata.name as $n | (.data // {}) | to_entries | .[]
    | select(.key == "LOG_FORMAT") | select(.value != "json") | $n + "/" + .key' 'LOG_FORMAT selalu json'
  _none "$ov" '.[] | select(.kind == "ConfigMap") | .metadata.name as $n | (.data // {}) | to_entries | .[]
    | select(.key | test("^(CORS_ALLOWED_ORIGINS|WS_ALLOWED_ORIGINS)$")) | .value | split(",") | .[]
    | select(test("^https://[^*/]+$") == false)' 'semua origin https tanpa wildcard'
  _none "$ov" '.[] | select(.kind == "ConfigMap") | .metadata.name as $n | (.data // {}) | to_entries | .[]
    | select(.key | test("^(REALTIME_PUBLIC_URL)$")) | .value | select(test("^wss://") == false)' 'REALTIME_PUBLIC_URL wss://'
  # env wajib prod/staging (non-secret) ada
  expect "$ov" ConfigMap kotoki-common-config '.data.PRIVACY_POLICY_VERSION' 2026-10-01
  expect "$ov" ConfigMap kotoki-api-config '.data | has("TRUSTED_PROXY_CIDRS")' true
  expect "$ov" ConfigMap kotoki-realtime-config '.data | has("TRUSTED_PROXY_CIDRS")' true
  expect "$ov" ConfigMap kotoki-api-config '.data | has("SMTP_HOST")' true
  expect "$ov" ConfigMap kotoki-api-config '.data | has("SMTP_USERNAME")' true
  expect "$ov" ConfigMap kotoki-api-config '.data.SMTP_PORT' 587
  expect "$ov" ConfigMap kotoki-api-config '.data.AUTH_REFRESH_COOKIE_SECURE' true

  # ---- TestOverlayDomainValues (spec §13.2.17): nilai persis
  expect "$ov" ConfigMap kotoki-api-config '.data.CORS_ALLOWED_ORIGINS' "$APP"
  expect "$ov" ConfigMap kotoki-realtime-config '.data.WS_ALLOWED_ORIGINS' "$APP"
  expect "$ov" ConfigMap kotoki-api-config '.data.REALTIME_PUBLIC_URL' "wss://$RT"
  expect "$ov" ConfigMap kotoki-api-config '.data.PASSWORD_RESET_URL_BASE' "$APP/reset-password"
  expect "$ov" ConfigMap kotoki-api-config '.data.MAIL_FROM' "$MAILF"
  expect "$ov" ConfigMap kotoki-api-config '.data | has("AUTH_REFRESH_COOKIE_DOMAIN")' true
  _none "$ov" '.[] | select(.kind == "ConfigMap" and .metadata.name == "kotoki-api-config") | .data.AUTH_REFRESH_COOKIE_DOMAIN | select(. != "")' \
    'AUTH_REFRESH_COOKIE_DOMAIN kosong (host-only)'

  # ---- image: prod = digest, staging = tag; tanpa :latest
  _none "$ov" '.[] | select(.metadata.namespace == "kotoki") | select(.kind == "Deployment" or .kind == "Job") | .metadata.name as $n
    | .spec.template.spec.containers[0].image | select(test("^ghcr.io/nabepi/kotoki-backend/(api|realtime|worker|migrate|seed)[:@]") == false)' \
    'image dari ghcr.io/nabepi/kotoki-backend'
  _none "$ov" '.[] | select(.metadata.namespace == "kotoki") | select(.kind == "Deployment" or .kind == "Job")
    | .spec.template.spec.containers[0].image | select(test(":latest$"))' 'tanpa :latest'
  if [ "$ov" = prod ]; then
    _none "$ov" '.[] | select(.metadata.namespace == "kotoki") | select(.kind == "Deployment" or .kind == "Job") | .metadata.name as $n
      | .spec.template.spec.containers[0].image | select(test("@sha256:[0-9a-f]{64}$") == false)' 'semua image prod memakai digest'
  else
    _none "$ov" '.[] | select(.metadata.namespace == "kotoki") | select(.kind == "Deployment" or .kind == "Job")
      | .spec.template.spec.containers[0].image | select(test("@sha256:"))' 'image staging memakai tag (bukan digest)'
  fi
  expect "$ov" Job kotoki-seed "$C.env[] | select(.name == \"KOTOKI_ENV\") | .value" "$ov"

  # ---- ExternalSecret x5 (§13.8.4): kunci persis per binary, tanpa Secret literal
  expect "$ov" ExternalSecret kotoki-api-secrets '.spec.data | length' 5
  _none "$ov" '[.[] | select(.kind == "ExternalSecret") | .metadata.name] | sort | join(",")
    | select(. != "kotoki-api-secrets,kotoki-keda-secrets,kotoki-migrate-secrets,kotoki-realtime-secrets,kotoki-worker-secrets")' \
    'tepat lima ExternalSecret'
  while IFS='|' read -r es keys; do
    expect "$ov" ExternalSecret "$es" '.metadata.namespace' kotoki
    expect "$ov" ExternalSecret "$es" '.spec.refreshInterval' 1h
    expect "$ov" ExternalSecret "$es" '.spec.secretStoreRef.name' kotoki-secrets
    expect "$ov" ExternalSecret "$es" '.spec.secretStoreRef.kind' ClusterSecretStore
    expect "$ov" ExternalSecret "$es" '.spec.target.name' "$es"
    expect "$ov" ExternalSecret "$es" '.spec.data | map(.secretKey) | sort | join(",")' "$keys"
    # remoteRef: path per lingkungan, properti = nama kunci, tidak ada nilai
    _none "$ov" '.[] | select(.kind == "ExternalSecret" and .metadata.name == "'"$es"'") | .spec.data[] | select(.remoteRef.key | test("^kotoki/'"$ov"'/[a-z]+$") == false) | .secretKey' \
      "$es remoteRef.key di kotoki/$ov/<binary>"
    _none "$ov" '.[] | select(.kind == "ExternalSecret" and .metadata.name == "'"$es"'") | .spec.data[] | select(.remoteRef.property != .secretKey) | .secretKey' \
      "$es remoteRef.property = secretKey"
    # tanpa nilai literal: tidak ada template/dataFrom/stringData; target hanya name/creationPolicy/deletionPolicy
    expect "$ov" ExternalSecret "$es" '.spec | keys | sort | join(",")' 'data,refreshInterval,secretStoreRef,target'
    expect "$ov" ExternalSecret "$es" '.spec.target | keys | sort | join(",")' 'creationPolicy,deletionPolicy,name'
done <<'EOS'
kotoki-api-secrets|AUTH_JWT_PREVIOUS_SECRET,AUTH_JWT_SECRET,DB_URL,REDIS_URL,SMTP_PASSWORD
kotoki-realtime-secrets|CARTESIA_API_KEY,DB_URL,DEEPSEEK_API_KEY,ELEVENLABS_API_KEY,OPENAI_API_KEY,REDIS_URL
kotoki-worker-secrets|DB_URL,DEEPSEEK_API_KEY,OPENAI_API_KEY,RABBITMQ_URL,REDIS_URL
kotoki-migrate-secrets|DB_MIGRATE_URL
kotoki-keda-secrets|host
EOS
  # kunci vendor: ElevenLabs/Cartesia hanya di realtime; api tanpa kunci vendor/RabbitMQ; tanpa Deepgram/Gemini
  _none "$ov" '.[] | select(.kind == "ExternalSecret") | select(.metadata.name != "kotoki-realtime-secrets") | .metadata.name as $n
    | .spec.data[].secretKey | select(test("^(CARTESIA|ELEVENLABS)_")) ' 'kunci ElevenLabs/Cartesia hanya di realtime'
  _none "$ov" '.[] | select(.kind == "ExternalSecret" and .metadata.name == "kotoki-api-secrets") | .spec.data[].secretKey
    | select(test("_API_KEY$|^RABBITMQ_URL$"))' 'api tanpa kunci vendor/RabbitMQ'
  CHECKS=$((CHECKS + 1))
  if grep -qiE 'deepgram|gemini' "$K8S_RENDER_DIR/$ov.yaml"; then _fail "$ov memuat DEEPGRAM/GEMINI"; else _ok "$ov tanpa DEEPGRAM/GEMINI"; fi
  # kunci vendor tidak ada di ConfigMap/env Deployment
  _none "$ov" '.[] | select(.kind == "ConfigMap") | .metadata.name as $n | (.data // {}) | to_entries | .[] | select(.key | test("_API_KEY$")) | $n + "/" + .key' \
    'tanpa *_API_KEY di ConfigMap'
  # setiap secretRef Deployment/Job punya ExternalSecret penghasilnya
  CHECKS=$((CHECKS + 1))
  used="$("$YQ" -r '.[] | select(.metadata.namespace == "kotoki") | select(.kind == "Deployment" or .kind == "Job") | .spec.template.spec.containers[0].envFrom[] | .secretRef.name | select(. != null)' "$(render "$ov")" | sort -u)" || used=yq-gagal
  have="$("$YQ" -r '.[] | select(.kind == "ExternalSecret") | .spec.target.name' "$(render "$ov")" | sort -u)" || have=yq-gagal
  missing="$(comm -23 <(printf '%s\n' "$used") <(printf '%s\n' "$have") | tr '\n' ' ')"
  if [ -z "$used" ] || [ -n "$missing" ]; then _fail "$ov secretRef tanpa ExternalSecret: ${missing:-(tidak ada secretRef)}"; else _ok "$ov semua secretRef dipenuhi ExternalSecret"; fi

  # ---- RabbitmqCluster (§13.1): 3 node, quorum, TLS; vhost/pengguna dirujuk ke docs/ops/rabbitmq.md
  R=kotoki-rabbitmq
  expect "$ov" RabbitmqCluster $R '.metadata.namespace' kotoki-data
  expect "$ov" RabbitmqCluster $R '.metadata.annotations["kotoki.io/ops-doc"]' docs/ops/rabbitmq.md
  CHECKS=$((CHECKS + 1))
  if [ -f "$ROOT/docs/ops/rabbitmq.md" ]; then _ok "$ov docs/ops/rabbitmq.md ada"; else _fail "$ov docs/ops/rabbitmq.md tidak ada"; fi
  expect "$ov" RabbitmqCluster $R '.spec.replicas' 3
  expect "$ov" RabbitmqCluster $R '.spec.resources.requests.cpu' 1
  expect "$ov" RabbitmqCluster $R '.spec.resources.requests.memory' 2Gi
  expect "$ov" RabbitmqCluster $R '.spec.resources.limits.cpu' 2
  expect "$ov" RabbitmqCluster $R '.spec.resources.limits.memory' 2Gi
  expect "$ov" RabbitmqCluster $R '.spec.persistence.storage' 20Gi
  expect "$ov" RabbitmqCluster $R '.spec.rabbitmq.additionalConfig | test("vm_memory_high_watermark.relative = 0.6")' true
  expect "$ov" RabbitmqCluster $R '.spec.rabbitmq.additionalConfig | test("disk_free_limit.absolute = 2GB")' true
  expect "$ov" RabbitmqCluster $R '.spec.rabbitmq.additionalPlugins | join(",")' rabbitmq_prometheus
  expect "$ov" RabbitmqCluster $R '.spec.tls.secretName' kotoki-rabbitmq-tls
  expect "$ov" RabbitmqCluster $R '.spec.tls.disableNonTLSListeners' true
  expect "$ov" RabbitmqCluster $R '.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[0].topologyKey' topology.kubernetes.io/zone
  # vhost/pengguna bukan milik overlay ini: tidak ada definisi pengguna/izin/override di RabbitmqCluster
  expect "$ov" RabbitmqCluster $R '.spec | keys | sort | join(",")' 'affinity,image,persistence,rabbitmq,replicas,resources,tls'
  expect "$ov" RabbitmqCluster $R '.spec.rabbitmq | keys | sort | join(",")' 'additionalConfig,additionalPlugins'
  expect "$ov" RabbitmqCluster $R '.spec.rabbitmq.additionalConfig | test("default_user|default_pass|load_definitions") == false' true
  expect "$ov" Certificate kotoki-rabbitmq '.metadata.namespace' kotoki-data
  expect "$ov" Certificate kotoki-rabbitmq '.spec.secretName' kotoki-rabbitmq-tls

  # ---- scaler: ScaledObject ada; realtime 3/20 (G4b butuh ~15 pod, sama dengan prod)
  expect "$ov" ScaledObject kotoki-realtime '.spec.minReplicaCount' 3
  expect "$ov" ScaledObject kotoki-realtime '.spec.maxReplicaCount' 20
  _none "$ov" '[.[] | select(.kind == "ScaledObject") | .metadata.name] | sort | join(",")
    | select(. != "kotoki-api,kotoki-realtime,kotoki-worker-evaluation,kotoki-worker-learner,kotoki-worker-memory")' 'lima ScaledObject'

  # ---- NetworkPolicy: ipBlock data store bukan lagi TEST-NET base (192.0.2.0/24); placeholder per overlay
  for np in kotoki-api kotoki-realtime kotoki-worker kotoki-jobs; do
    expect "$ov" NetworkPolicy $np '.spec.egress[0].to[0].ipBlock.cidr != "192.0.2.0/24"' true
    expect "$ov" NetworkPolicy $np '.spec.egress[0].to[0].ipBlock.cidr | test("^(REPLACE_DATA_STORE_CIDR|[0-9]+[.][0-9]+[.][0-9]+[.][0-9]+/[0-9]+)$")' true
  done
  _none "$ov" '.[] | select(.kind == "NetworkPolicy") | .spec.egress[]? | (.to // [])[] | select(.ipBlock.cidr == "192.0.2.0/24")' 'tanpa CIDR TEST-NET-1 base di NetworkPolicy'

  # ---- recording rule infra per lingkungan + penjaga exporter (alert pg_up/redis_up tidak boleh buta)
  expect "$ov" PrometheusRule kotoki-infra-recording '.metadata.namespace' observability
  expect "$ov" PrometheusRule kotoki-infra-recording '[.spec.groups[].rules[] | select(.record != null) | .record] | sort | join(",")' \
    'kotoki:postgres_cpu_utilization:ratio,kotoki:redis_memory_utilization:ratio'
  expect "$ov" PrometheusRule kotoki-infra-recording '[.spec.groups[].rules[] | select(.alert != null) | .alert] | sort | join(",")' \
    'InfraRecordingRuleMissing,PostgresExporterMissing,RedisExporterMissing'
  expect "$ov" PrometheusRule kotoki-infra-recording '.spec.groups[].rules[] | select(.alert == "PostgresExporterMissing") | .expr' 'absent(pg_up)'
  expect "$ov" PrometheusRule kotoki-infra-recording '.spec.groups[].rules[] | select(.alert == "RedisExporterMissing") | .expr' 'absent(redis_up)'
  # MANUAL GATE: ekspresi record masih placeholder REPLACE_CLOUD_METRIC (pins-check --prod menolaknya) atau sudah dipetakan
  _none "$ov" '.[] | select(.kind == "PrometheusRule" and .metadata.name == "kotoki-infra-recording") | .spec.groups[].rules[] | select(.record != null)
    | select((.expr | length) == 0) | .record' 'recording rule infra punya ekspresi'
  # ---- keputusan vendor provider: penanda ada (pins-check --prod menolak selama masih placeholder)
  expect "$ov" ConfigMap kotoki-provider-config '.metadata.annotations["kotoki.io/provider-selection"] != null' true
done

# ---- pins-check: mode default lulus; --prod sengaja gagal sampai MANUAL GATE dan menyebut kedua overlay
CHECKS=$((CHECKS + 1))
if "$ROOT/deploy/k8s/hack/pins-check.sh" >/dev/null 2>&1; then _ok "pins-check (default) rc 0"; else _fail "pins-check (default) harus lulus"; fi
CHECKS=$((CHECKS + 1))
pins_out="$("$ROOT/deploy/k8s/hack/pins-check.sh" --prod 2>&1)"; pins_rc=$?
if [ "$pins_rc" -ne 0 ] && grep -q 'overlay staging memuat' <<<"$pins_out" && grep -q 'overlay prod memuat' <<<"$pins_out"; then
  _ok "pins-check --prod menolak placeholder staging dan prod (sengaja, sampai MANUAL GATE Task 24)"
else
  _fail "pins-check --prod harus gagal dan menyebut staging serta prod (rc=$pins_rc)"
fi
for pat in 'REPLACE_SHA' 'REPLACE_DATA_STORE_CIDR' '198.51.100' 'REPLACE_PRODUCT_OWNER_DECISION' 'REPLACE_CLOUD_METRIC' 'REPLACE_SMTP_HOST'; do
  CHECKS=$((CHECKS + 1))
  if grep -q "$pat" <<<"$pins_out"; then _ok "pins-check --prod mendeteksi $pat"; else _fail "pins-check --prod tidak mendeteksi $pat"; fi
done
CHECKS=$((CHECKS + 1))
if grep -q 'sha256:0000000000000000000000000000000000000000000000000000000000000000' "$K8S_RENDER_DIR/prod.yaml"; then _ok "prod memuat digest nol placeholder (ditolak pins-check --prod)"; else _fail "prod tanpa digest nol placeholder"; fi

finish
