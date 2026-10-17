#!/usr/bin/env bash
# Overlay dev (spec §13.10.8, M10 Task 10): 1 replika tiap Deployment, tanpa ScaledObject,
# PROVIDER_MODE=fake, Secret dev (nilai dummy, hanya di dev), dependency in-cluster di namespace
# kotoki-data. Invarian lintas-komponen dijalankan oleh 00-common.sh dev (profil dev) dan edge/jobs/
# observability oleh 40/50/60 dev; skrip ini memeriksa apa yang khas dev.
# Pemakaian: 70-overlay-dev.sh [target...]   (default: dev)
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../hack/lib.sh
source "$HERE/../hack/lib.sh"

targets=("$@"); [ "${#targets[@]}" -gt 0 ] || targets=(dev)
POD='.spec.template.spec'
C="$POD.containers[0]"
COMP='app.kubernetes.io/component'

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
  # ---- profil invarian lintas-komponen dan area lain
  for s in 00-common 40-jobs 50-edge 60-observability; do
    bash "$HERE/$s.sh" "$ov" >/dev/null || _fail "$ov $s.sh gagal (jalankan: bash deploy/k8s/assert/$s.sh $ov)"
    CHECKS=$((CHECKS + 1))
  done

  # ---- 1 replika tiap Deployment; tidak ada ScaledObject
  for w in api realtime worker-core worker-evaluation worker-memory worker-learner; do
    expect "$ov" Deployment "kotoki-$w" '.spec.replicas' 1
    # PDB dipertahankan (maxUnavailable 1 sah pada 1 replika: tidak pernah memblokir drain)
    expect "$ov" PodDisruptionBudget "kotoki-$w" '.spec.maxUnavailable' 1
  done
  _none "$ov" '.[] | select(.kind == "ScaledObject") | .metadata.name' 'tidak ada ScaledObject'
  # dev berjalan di klaster tanpa node pool realtime dan tanpa zona
  expect "$ov" Deployment kotoki-realtime "$POD.nodeSelector // \"none\"" none
  expect "$ov" Deployment kotoki-realtime "$POD.tolerations // \"none\"" none
  expect "$ov" Deployment kotoki-realtime "$C.resources.limits.memory" 2Gi
  expect "$ov" Deployment kotoki-realtime "$C.env[] | select(.name == \"GOMEMLIMIT\") | .value" 1800MiB

  # ---- ConfigMap dev
  expect "$ov" ConfigMap kotoki-common-config '.data.APP_ENV' dev
  expect "$ov" ConfigMap kotoki-common-config '.data.LOG_FORMAT' json
  expect "$ov" ConfigMap kotoki-common-config '.data.OTEL_EXPORTER_OTLP_ENDPOINT' 'http://otel-collector.observability:4317'
  expect "$ov" ConfigMap kotoki-provider-config '.data.PROVIDER_MODE' fake
  expect "$ov" ConfigMap kotoki-provider-config '.data.FAKE_PRIMARY_STT_LATENCY_MS' 250
  expect "$ov" ConfigMap kotoki-provider-config '.data.FAKE_PRIMARY_LLM_LATENCY_MS' 500
  expect "$ov" ConfigMap kotoki-provider-config '.data.FAKE_LLM_TOKEN_INTERVAL_MS' 30
  expect "$ov" ConfigMap kotoki-provider-config '.data.FAKE_PRIMARY_TTS_LATENCY_MS' 300
  expect "$ov" ConfigMap kotoki-realtime-config '.data.WS_ALLOW_EMPTY_ORIGIN' true
  expect "$ov" ConfigMap kotoki-api-config '.data.MAIL_MODE' log
  expect "$ov" ConfigMap kotoki-api-config '.data.REALTIME_PUBLIC_URL' 'wss://realtime.dev.kotoki.id'
  expect "$ov" ConfigMap kotoki-provider-config '.metadata.namespace' kotoki
  # semua workload yang membaca konfigurasi provider memakai ConfigMap yang sama (§13.2.8)
  for d in api realtime worker-core worker-evaluation worker-memory worker-learner; do
    expect "$ov" Deployment "kotoki-$d" "$C.envFrom | map(select(.configMapRef.name == \"kotoki-provider-config\")) | length" 1
  done

  # ---- Secret dev: lima, nilai dummy, hanya di dev (kunci persis; tanpa kunci vendor di mode fake)
  _none "$ov" '[.[] | select(.kind == "Secret" and .metadata.namespace == "kotoki") | .metadata.name] | sort | join(",")
    | select(. != "kotoki-api-secrets,kotoki-keda-secrets,kotoki-migrate-secrets,kotoki-realtime-secrets,kotoki-worker-secrets") | "Secret kotoki: " + .' \
    'Secret namespace kotoki tepat lima'
  while IFS='|' read -r s keys; do
    expect "$ov" Secret "$s" '.type' Opaque
    expect "$ov" Secret "$s" '.data | keys | sort | join(",")' "$keys"
  done <<'EOS'
kotoki-api-secrets|AUTH_JWT_SECRET,DB_URL,REDIS_URL
kotoki-realtime-secrets|DB_URL,REDIS_URL
kotoki-worker-secrets|DB_URL,RABBITMQ_URL,REDIS_URL
kotoki-migrate-secrets|DB_MIGRATE_URL
kotoki-keda-secrets|host
EOS
  # setiap secretRef/configMapRef Deployment/Job terpenuhi oleh objek yang dirender
  CHECKS=$((CHECKS + 1))
  used="$("$YQ" -r '.[] | select(.metadata.namespace == "kotoki") | select(.kind == "Deployment" or .kind == "Job") | .spec.template.spec.containers[0].envFrom[] | (.secretRef.name // .configMapRef.name)' "$(render "$ov")" | sort -u)" || used=yq-gagal
  have="$("$YQ" -r '.[] | select(.metadata.namespace == "kotoki") | select(.kind == "Secret" or .kind == "ConfigMap") | .metadata.name' "$(render "$ov")" | sort -u)" || have=yq-gagal
  missing="$(comm -23 <(printf '%s\n' "$used") <(printf '%s\n' "$have") | tr '\n' ' ')"
  if [ -z "$used" ] || [ -n "$missing" ]; then _fail "$ov envFrom merujuk objek yang tidak dirender: ${missing:-(tidak ada envFrom)}"; else _ok "$ov semua envFrom merujuk objek yang ada"; fi
  # nilai dev jelas dummy dan tidak mengarah ke luar klaster
  _none "$ov" '.[] | select(.kind == "Secret") | select(.metadata.namespace == "kotoki") | .metadata.name as $n | (.data // {}) | to_entries | .[]
    | select(.key | test("_URL$|^host$")) | select((.value | sub("\s"; "") | @base64d | test("sslmode=disable|amqp://|redis://")) == false) | .key' \
    'URL Secret dev tanpa TLS dan menunjuk dependency in-cluster'
  _none "$ov" '.[] | select(.kind == "Secret") | select(.metadata.namespace == "kotoki") | (.data // {}) | to_entries | .[]
    | select(.key != "AUTH_JWT_SECRET") | select(.key != "REDIS_URL") | select((.value | sub("\s"; "") | @base64d | test("dev-only")) == false) | .key' \
    'setiap nilai Secret dev (kecuali REDIS_URL tanpa kredensial) memuat penanda dev-only'
  _none "$ov" '.[] | select(.kind == "Secret") | select(.metadata.namespace == "kotoki") | (.data // {}) | to_entries | .[] | select(.key == "REDIS_URL")
    | (.value | sub("\s"; "") | @base64d) | select(. != "redis://redis.kotoki-data:6379/0")' \
    'REDIS_URL dev = redis://redis.kotoki-data:6379/0 tanpa kredensial'
  # AUTH_JWT_SECRET: base64 dua lapis (Secret base64 dari string base64 dari penanda dev-only), >= 32 byte
  _none "$ov" '.[] | select(.kind == "Secret") | select(.metadata.name == "kotoki-api-secrets") | .data.AUTH_JWT_SECRET | sub("\s"; "") | @base64d | sub("\s"; "") | @base64d
    | select(test("dev-only") == false)' \
    'AUTH_JWT_SECRET dev berpenanda dev-only'
  _none "$ov" '.[] | select(.kind == "Secret") | select(.metadata.name == "kotoki-api-secrets") | .data.AUTH_JWT_SECRET | sub("\s"; "") | @base64d | sub("\s"; "") | @base64d
    | select(length < 32)' \
    'AUTH_JWT_SECRET dev >= 32 byte setelah decode'

  # ---- dependency in-cluster: namespace kotoki-data
  expect "$ov" Namespace kotoki-data '.metadata.name' kotoki-data
  expect "$ov" StatefulSet postgres '.metadata.namespace' kotoki-data
  expect "$ov" StatefulSet postgres '.spec.replicas' 1
  expect "$ov" StatefulSet postgres "$POD.containers[0].image" 'pgvector/pgvector:pg17'
  expect "$ov" StatefulSet redis '.metadata.namespace' kotoki-data
  expect "$ov" StatefulSet redis '.spec.replicas' 1
  expect "$ov" StatefulSet redis "$POD.containers[0].image" 'redis:7.4'
  expect "$ov" StatefulSet rabbitmq '.metadata.namespace' kotoki-data
  expect "$ov" StatefulSet rabbitmq '.spec.replicas' 1
  expect "$ov" StatefulSet rabbitmq "$POD.containers[0].image" 'rabbitmq:4-management'
  for s in postgres redis rabbitmq; do
    expect "$ov" Service "$s" '.metadata.namespace' kotoki-data
  done
  expect "$ov" Service postgres '.spec.ports[] | select(.name == "postgres") | .port' 5432
  expect "$ov" Service redis '.spec.ports[] | select(.name == "redis") | .port' 6379
  expect "$ov" Service rabbitmq '.spec.ports[] | select(.name == "amqp") | .port' 5672
  # vhost /kotoki dan tiga pengguna (docs/ops/rabbitmq.md)
  expect "$ov" Secret rabbitmq-definitions '.metadata.namespace' kotoki-data
  _none "$ov" '.[] | select(.kind == "Secret") | select(.metadata.name == "rabbitmq-definitions") | .data["definitions.json"] | sub("\s"; "") | @base64d | from_json
    | [.vhosts[].name] | sort | join(",") | select(. != "/kotoki")' \
    'RabbitMQ dev: vhost /kotoki'
  _none "$ov" '.[] | select(.kind == "Secret") | select(.metadata.name == "rabbitmq-definitions") | .data["definitions.json"] | sub("\s"; "") | @base64d | from_json
    | [.users[].name] | sort | join(",") | select(. != "admin,keda,kotoki-worker")' \
    'RabbitMQ dev: pengguna admin,keda,kotoki-worker'
  # semua objek yang berpindah namespace berada di himpunan yang dikenal; objek dependency di kotoki-data
  _none "$ov" '.[] | select(.metadata.namespace != null) | select((.metadata.namespace | test("^(kotoki|kotoki-data|observability|envoy-gateway-system)$")) == false)
    | .kind + "/" + .metadata.name + " ns=" + .metadata.namespace' \
    'tidak ada namespace tak dikenal (mis. default)'
  _none "$ov" '.[] | select((.kind == "StatefulSet") or ((.kind == "Service") and ((.metadata.name == "postgres") or (.metadata.name == "redis") or (.metadata.name == "rabbitmq"))))
    | select(.metadata.namespace != "kotoki-data") | .kind + "/" + .metadata.name' \
    'StatefulSet dan Service dependency di kotoki-data'
  _none "$ov" '.[] | select(.kind == "Deployment" or .kind == "Job" or .kind == "ConfigMap" or .kind == "Secret" or .kind == "StatefulSet" or .kind == "Service" or .kind == "PodDisruptionBudget" or .kind == "NetworkPolicy")
    | select(.metadata.namespace == null) | .kind + "/" + .metadata.name' \
    'semua objek berskop namespace punya namespace eksplisit'
  # exporter pg_up / redis_up + recording rule per lingkungan (alert pg_up/redis_up tidak boleh buta)
  expect "$ov" StatefulSet postgres "$POD.containers[] | select(.name == \"exporter\") | .ports[0].name" pg-metrics
  expect "$ov" StatefulSet redis "$POD.containers[] | select(.name == \"exporter\") | .ports[0].name" redis-metrics
  expect "$ov" PodMonitor kotoki-data '.metadata.namespace' observability
  expect "$ov" PodMonitor kotoki-data '.spec.namespaceSelector.matchNames[0]' kotoki-data
  expect "$ov" PrometheusRule kotoki-infra-recording '.metadata.namespace' observability
  expect "$ov" PrometheusRule kotoki-infra-recording '[.spec.groups[].rules[] | select(.record != null) | .record] | sort | join(",")' \
    'kotoki:postgres_cpu_utilization:ratio,kotoki:redis_memory_utilization:ratio'
  _none "$ov" '.[] | select(.kind == "PrometheusRule" and .metadata.name == "kotoki-infra-recording") | .spec.groups[].rules[] | select(.expr | test("REPLACE_")) | .record' \
    'recording rule dev tanpa REPLACE_'

  # ---- NetworkPolicy dev: data store lewat namespaceSelector kotoki-data (bukan ipBlock placeholder)
  for c in api realtime worker-core worker-evaluation migrate seed; do
    CHECKS=$((CHECKS + 1))
    got="$(C="$c" "$YQ" -r '[.[] | select(.kind == "NetworkPolicy") | select((.spec.podSelector.matchLabels["'"$COMP"'"] == strenv(C)) or ((.spec.podSelector.matchExpressions // []) | map(select(.key == "'"$COMP"'") | .values[]) | any_c(. == strenv(C))))
      | .spec.egress[] | select((.ports // []) | map(select(.port == 5432)) | length > 0) | .to[] | .namespaceSelector.matchLabels["kubernetes.io/metadata.name"]] | unique | join(",")' "$(render "$ov")")" || got=yq-gagal
    if [ "$got" != kotoki-data ]; then _fail "$ov $c PostgreSQL: want kotoki-data, got '$got'"; else _ok "$ov $c PostgreSQL via namespaceSelector kotoki-data"; fi
  done

  # ---- placeholder base diganti; tag dev; hostname dev
  CHECKS=$((CHECKS + 1))
  if grep -q 'REPLACE_\|192\.0\.2\.' "$K8S_RENDER_DIR/$ov.yaml"; then
    _fail "$ov: placeholder tertinggal ($(grep -o 'REPLACE_[A-Z_]*\|192\.0\.2\.[0-9/]*' "$K8S_RENDER_DIR/$ov.yaml" | sort -u | tr '\n' ' '))"
  else _ok "$ov tanpa REPLACE_ dan 192.0.2.*"; fi
  _none "$ov" '.[] | select((.metadata.namespace == "kotoki") and ((.kind == "Deployment") or (.kind == "Job"))) | .metadata.name as $n
    | .spec.template.spec.containers[0].image | select(test("^ghcr.io/nabepi/kotoki-backend/[a-z]+:dev$") == false)' \
    'image tag dev'
  _none "$ov" '.[] | select(.metadata.namespace == "kotoki") | select(.kind == "Deployment" or .kind == "Job") | select(.metadata.labels["app.kubernetes.io/version"] != "dev") | .metadata.name' \
    'label versi dev (metadata)'
  _none "$ov" '.[] | select(.metadata.namespace == "kotoki") | select(.kind == "Deployment" or .kind == "Job") | select(.spec.template.metadata.labels["app.kubernetes.io/version"] != "dev") | .metadata.name' \
    'label versi dev (template pod)'
  expect "$ov" Job kotoki-migrate '.metadata.namespace' kotoki
  expect "$ov" Job kotoki-seed "$C.env[] | select(.name == \"KOTOKI_ENV\") | .value" dev
  expect "$ov" Job kotoki-migrate "$C.envFrom[0].secretRef.name" kotoki-migrate-secrets
done

finish
