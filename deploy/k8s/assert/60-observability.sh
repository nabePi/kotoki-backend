#!/usr/bin/env bash
# Observability (spec §13.1, §13.7.1-13.7.2; M10 Task 7): PodMonitor, collector OTel dua tier
# (lb -> sampler dengan tail sampling) dan aturan log audit Alloy/Loki. Alert (Task 8) memakai
# berkasnya sendiri; skrip ini hanya memeriksa objek Task 7.
# Pemakaian: 60-observability.sh [target...]   (default: base)
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../hack/lib.sh
source "$HERE/../hack/lib.sh"

targets=("$@"); [ "${#targets[@]}" -gt 0 ] || targets=(base)
POD='.spec.template.spec'
C="$POD.containers[0]"
NS=observability
CFG_TMP="$(mktemp)"; trap 'rm -f "$CFG_TMP"' EXIT

# cfg_load <overlay> <ConfigMap> <key>: isi kunci ConfigMap -> $CFG_TMP; gagal keras bila kosong.
cfg_load() {
  local j out
  j="$(render "$1")" || return 1
  out="$(CM="$2" KEY="$3" "$YQ" -r '.[] | select(.kind == "ConfigMap" and .metadata.name == strenv(CM)) | .data[strenv(KEY)]' "$j")" || return 1
  [ -n "$out" ] && [ "$out" != null ] || return 1
  printf '%s\n' "$out" >"$CFG_TMP"
}

# expect_cfg <overlay> <ConfigMap> '<yq pada config.yaml>' '<want>'
expect_cfg() {
  local ov="$1" cm="$2" expr="$3" want="$4" got
  CHECKS=$((CHECKS + 1))
  if ! cfg_load "$ov" "$cm" config.yaml; then _fail "$ov ConfigMap/$cm data[config.yaml] tidak ada atau kosong"; return 1; fi
  got="$("$YQ" -r "$expr" "$CFG_TMP" 2>&1)" || { _fail "$ov $cm $expr: yq gagal: $got"; return 1; }
  if [ -z "$got" ] || [ "$got" = null ]; then _fail "$ov $cm $expr: kosong"; return 1; fi
  if [ "$got" != "$want" ]; then _fail "$ov $cm $expr: want '$want', got '$got'"; return 1; fi
  _ok "$ov $cm $expr = $want"
}

# expect_text_absent <overlay> <ConfigMap> <key> '<regex i>' <label>: kunci tidak boleh cocok.
expect_text_absent() {
  local ov="$1" cm="$2" key="$3" re="$4" label="$5"
  CHECKS=$((CHECKS + 1))
  if ! cfg_load "$ov" "$cm" "$key"; then _fail "$ov ConfigMap/$cm data[$key] tidak ada atau kosong"; return 1; fi
  if grep -Eiq "$re" "$CFG_TMP"; then _fail "$ov $cm/$key $label: cocok $re"; return 1; fi
  _ok "$ov $cm/$key $label"
}

# expect_text <overlay> <ConfigMap> <key> '<regex>' <label>: kunci harus cocok.
expect_text() {
  local ov="$1" cm="$2" key="$3" re="$4" label="$5"
  CHECKS=$((CHECKS + 1))
  if ! cfg_load "$ov" "$cm" "$key"; then _fail "$ov ConfigMap/$cm data[$key] tidak ada atau kosong"; return 1; fi
  if ! grep -Eq "$re" "$CFG_TMP"; then _fail "$ov $cm/$key $label: tidak cocok $re"; return 1; fi
  _ok "$ov $cm/$key $label"
}

# Pin image collector harus sama dengan docker-compose.yml (M1).
COMPOSE_IMG="$(grep -oE 'otel/opentelemetry-collector-contrib:[0-9][0-9.]*' "$ROOT/docker-compose.yml" | sort -u)"
if [ "$(wc -l <<<"$COMPOSE_IMG")" -ne 1 ] || [ -z "$COMPOSE_IMG" ]; then
  echo "60-observability: pin image collector di docker-compose.yml tidak tunggal: '$COMPOSE_IMG'" >&2; exit 1
fi

for ov in "${targets[@]}"; do
  # ---- PodMonitor kotoki: scrape /metrics port ops tiap 15 s dari namespace kotoki
  P='.spec'
  expect "$ov" PodMonitor kotoki '.metadata.namespace' $NS
  expect "$ov" PodMonitor kotoki "$P.namespaceSelector.matchNames | join(\",\")" kotoki
  expect "$ov" PodMonitor kotoki "$P.selector.matchLabels | keys | join(\",\")" app.kubernetes.io/name
  expect "$ov" PodMonitor kotoki "$P.selector.matchLabels[\"app.kubernetes.io/name\"]" kotoki
  expect "$ov" PodMonitor kotoki "$P.podMetricsEndpoints | length" 1
  expect "$ov" PodMonitor kotoki "$P.podMetricsEndpoints[0].port" ops
  expect "$ov" PodMonitor kotoki "$P.podMetricsEndpoints[0].path" /metrics
  expect "$ov" PodMonitor kotoki "$P.podMetricsEndpoints[0].interval" 15s
  # label `job` milik kotoki_job_* (session_sweeper, ...) harus menang atas label target `job`;
  # tanpa honorLabels ia menjadi exported_job dan alert Sweeper/Cleanup tidak pernah cocok.
  expect "$ov" PodMonitor kotoki "$P.podMetricsEndpoints[0].honorLabels" true
  expect "$ov" PodMonitor kotoki "$P.podTargetLabels | join(\",\")" app.kubernetes.io/component
  expect "$ov" PodMonitor kotoki '.metadata.labels.release' kube-prometheus-stack

  # ---- PrometheusRule kotoki-alerts: 34 alert bernama persis hack/alert-names.txt (isi ekspresi: make prom-check
  # dan test/tools/metricref); di sini hanya keberadaan objek yang di-render ke manifest.
  expect "$ov" PrometheusRule kotoki-alerts '.metadata.namespace' $NS
  expect "$ov" PrometheusRule kotoki-alerts '.metadata.labels.release' kube-prometheus-stack
  expect "$ov" PrometheusRule kotoki-alerts '[.spec.groups[].rules[]] | length' 34
  expect "$ov" PrometheusRule kotoki-alerts '[.spec.groups[].rules[].alert] | join(",")' "$(paste -sd, "$HERE/../hack/alert-names.txt")"

  # ---- Deployment: hanya kotoki-* di namespace kotoki dan dua collector di observability
  CHECKS=$((CHECKS + 1))
  stray="$("$YQ" -r '.[] | select(.kind == "Deployment") | select(((.metadata.namespace == "kotoki" and (.metadata.name | test("^kotoki-"))) or (.metadata.namespace == "observability" and (.metadata.name == "otel-collector-lb" or .metadata.name == "otel-collector-sampler"))) | not) | .metadata.namespace + "/" + .metadata.name' "$(render "$ov")")" || stray="yq-gagal"
  if [ -n "$stray" ]; then _fail "$ov Deployment tak dikenal: $(tr '\n' ' ' <<<"$stray")"; else _ok "$ov Deployment hanya kotoki-* dan dua collector"; fi

  # ---- Collector: kedua tier (TestCollectorHardened = setara 00-common)
  for d in otel-collector-lb otel-collector-sampler; do
    comp="${d#otel-collector-}"
    expect "$ov" Deployment $d '.metadata.namespace' $NS
    expect "$ov" Deployment $d '.spec.replicas' 2
    expect "$ov" Deployment $d '.metadata.labels["app.kubernetes.io/name"]' otel-collector
    expect "$ov" Deployment $d '.metadata.labels["app.kubernetes.io/component"]' "$comp"
    expect "$ov" Deployment $d '.spec.selector.matchLabels["app.kubernetes.io/component"]' "$comp"
    expect "$ov" Deployment $d '.spec.template.metadata.labels["app.kubernetes.io/component"]' "$comp"
    expect "$ov" Deployment $d '.spec.strategy.rollingUpdate.maxUnavailable' 0
    expect "$ov" Deployment $d "$C.image" "$COMPOSE_IMG"
    expect "$ov" Deployment $d "$C.args | length" 1
    expect "$ov" Deployment $d "$C.args[0]" '--config=/etc/otelcol/config.yaml'
    expect "$ov" Deployment $d "$POD.automountServiceAccountToken" false
    expect "$ov" Deployment $d "$POD.securityContext.runAsNonRoot" true
    expect "$ov" Deployment $d "$POD.securityContext.runAsUser" 65532
    expect "$ov" Deployment $d "$POD.securityContext.seccompProfile.type" RuntimeDefault
    expect "$ov" Deployment $d "$POD.priorityClassName" kotoki-default
    expect "$ov" Deployment $d "$C.securityContext.allowPrivilegeEscalation" false
    expect "$ov" Deployment $d "$C.securityContext.readOnlyRootFilesystem" true
    expect "$ov" Deployment $d "$C.securityContext.capabilities.drop | join(\",\")" ALL
    expect "$ov" Deployment $d "$POD.containers | length" 1
    expect "$ov" Deployment $d "$C.envFrom // \"none\"" none        # tanpa secret/env bersama
    expect "$ov" Deployment $d "$C.env | map(select(.valueFrom != null)) | length" 0
    expect "$ov" Deployment $d "$C.env[] | select(.name == \"GOMEMLIMIT\") | .value | test(\"^[0-9]+MiB\$\")" true
    expect "$ov" Deployment $d "$C.volumeMounts[] | select(.name == \"tmp\") | .mountPath" /tmp
    expect "$ov" Deployment $d "$POD.volumes[] | select(.name == \"tmp\") | .emptyDir.sizeLimit" 64Mi
    expect "$ov" Deployment $d "$C.volumeMounts[] | select(.name == \"config\") | .mountPath" /etc/otelcol
    expect "$ov" Deployment $d "$C.volumeMounts[] | select(.name == \"config\") | .readOnly" true
    expect "$ov" Deployment $d "$POD.volumes[] | select(.name == \"config\") | .configMap.name" "$d-config"
    expect "$ov" Deployment $d "$C.ports[] | select(.name == \"health\") | .containerPort" 13133
    expect "$ov" Deployment $d "$C.livenessProbe.httpGet.port" health
    expect "$ov" Deployment $d "$C.readinessProbe.httpGet.port" health
    expect "$ov" Deployment $d "$C.resources.requests.cpu != null" true
    expect "$ov" Deployment $d "$C.resources.requests.memory != null" true
    expect "$ov" Deployment $d "$C.resources.limits.memory != null" true
    expect "$ov" PodDisruptionBudget $d '.spec.maxUnavailable' 1
    expect "$ov" PodDisruptionBudget $d '.spec.selector.matchLabels["app.kubernetes.io/component"]' "$comp"
    expect "$ov" PodDisruptionBudget $d '.metadata.namespace' $NS

    # Tidak ada rahasia/PII di konfigurasi collector; tidak ada exporter debug/logging (span ke stdout)
    expect_text_absent "$ov" "$d-config" config.yaml 'password|passwd|token|api[_-]?key|authorization|bearer|secret|://[^/@ ]+:[^/@ ]+@' "tanpa kredensial"
    expect_cfg "$ov" "$d-config" '.exporters | keys | map(select(test("^(debug|logging|file)"))) | length' 0
    expect_cfg "$ov" "$d-config" '.processors | keys | map(select(test("^(attributes|resource|transform)"))) | length' 0
    # setiap pipeline diawali memory_limiter
    expect_cfg "$ov" "$d-config" '.service.pipelines | to_entries | map(select(.value.processors[0] != "memory_limiter")) | length' 0
    expect_cfg "$ov" "$d-config" '.processors.memory_limiter.check_interval' 1s
    expect_cfg "$ov" "$d-config" '.processors.memory_limiter.limit_mib != null and .processors.memory_limiter.spike_limit_mib != null' true
    expect_cfg "$ov" "$d-config" '.extensions.health_check.endpoint' 0.0.0.0:13133
    expect_cfg "$ov" "$d-config" '.service.extensions | join(",")' health_check
    expect_cfg "$ov" "$d-config" '.service.pipelines | keys | join(",")' traces
    expect_cfg "$ov" "$d-config" '.receivers.otlp.protocols.grpc.endpoint' 0.0.0.0:4317
    expect_cfg "$ov" "$d-config" '.service.pipelines.traces.receivers | join(",")' otlp
  done

  # ---- tier lb: routing per traceID ke sampler (tail sampling tanpa routing memecah trace)
  L=otel-collector-lb-config
  expect_cfg "$ov" $L '.receivers.otlp.protocols.http.endpoint' 0.0.0.0:4318
  expect_cfg "$ov" $L '.exporters.loadbalancing.routing_key' traceID
  expect_cfg "$ov" $L '.exporters.loadbalancing.resolver.dns.hostname' otel-collector-sampler-headless.observability
  expect_cfg "$ov" $L '.exporters.loadbalancing.resolver.dns.port' 4317
  expect_cfg "$ov" $L '.exporters.loadbalancing.protocol.otlp.tls.insecure' true
  expect_cfg "$ov" $L '.service.pipelines.traces.processors | join(",")' memory_limiter,batch
  expect_cfg "$ov" $L '.service.pipelines.traces.exporters | join(",")' loadbalancing
  expect_cfg "$ov" $L '.processors | has("tail_sampling")' false

  # ---- tier sampler: tail_sampling 100% error, 100% > 2 s, 10% sisanya
  S=otel-collector-sampler-config
  expect_cfg "$ov" $S '.processors.tail_sampling.decision_wait' 10s
  expect_cfg "$ov" $S '.processors.tail_sampling.policies | length' 3
  expect_cfg "$ov" $S '.processors.tail_sampling.policies | map(.type) | join(",")' status_code,latency,probabilistic
  expect_cfg "$ov" $S '.processors.tail_sampling.policies[] | select(.type == "status_code") | .status_code.status_codes | join(",")' ERROR
  expect_cfg "$ov" $S '.processors.tail_sampling.policies[] | select(.type == "latency") | .latency.threshold_ms' 2000
  expect_cfg "$ov" $S '.processors.tail_sampling.policies[] | select(.type == "probabilistic") | .probabilistic.sampling_percentage' 10
  expect_cfg "$ov" $S '.service.pipelines.traces.processors | join(",")' memory_limiter,tail_sampling,batch
  expect_cfg "$ov" $S '.service.pipelines.traces.exporters | join(",")' otlp/tempo
  expect_cfg "$ov" $S '.exporters["otlp/tempo"].endpoint' tempo.observability:4317
  expect_cfg "$ov" $S '.receivers | keys | join(",")' otlp
  expect "$ov" Deployment otel-collector-sampler "$C.resources.limits.memory" 1Gi

  # ---- Service: otel-collector = target aplikasi (lb); headless sampler = resolver DNS lb
  expect "$ov" Service otel-collector '.metadata.namespace' $NS
  expect "$ov" Service otel-collector '.spec.selector["app.kubernetes.io/component"]' lb
  expect "$ov" Service otel-collector '.spec.selector["app.kubernetes.io/name"]' otel-collector
  expect "$ov" Service otel-collector '.spec.ports | map(.name + ":" + (.port | tostring)) | join(",")' otlp-grpc:4317,otlp-http:4318
  expect "$ov" Service otel-collector '.spec.clusterIP // "none"' none
  expect "$ov" Service otel-collector-sampler-headless '.metadata.namespace' $NS
  expect "$ov" Service otel-collector-sampler-headless '.spec.clusterIP' None
  expect "$ov" Service otel-collector-sampler-headless '.spec.selector["app.kubernetes.io/component"]' sampler
  expect "$ov" Service otel-collector-sampler-headless '.spec.ports | map(.name + ":" + (.port | tostring)) | join(",")' otlp-grpc:4317

  # ---- Aturan log audit: stream audit di Loki, retensi 400 hari (app 30 hari); tanpa PII
  A=alloy-audit-rule
  expect "$ov" ConfigMap $A '.metadata.namespace' $NS
  expect_text "$ov" $A audit.alloy 'audit' 'memuat audit'
  expect_text "$ov" $A audit.alloy 'stream *= *"audit"' 'label stream=audit'
  expect_text "$ov" $A audit.alloy 'audit[^:]*:true' 'cocok baris slog audit=true'
  expect_text "$ov" $A audit.alloy '400' 'retensi 400 hari dicatat'
  expect_text "$ov" $A audit.alloy '30' 'retensi app 30 hari dicatat'
  expect_text_absent "$ov" $A audit.alloy 'user_id|email|\bip\b|user_agent|token|password' 'tidak mengekstrak PII/kredensial ke label'
  CHECKS=$((CHECKS + 1))
  if cfg_load "$ov" $A loki-retention.yaml; then
    got="$("$YQ" -r '.limits_config.retention_period + "|" + (.limits_config.retention_stream | map(.selector + "=" + .period) | join(","))' "$CFG_TMP" 2>&1)"
    want='720h|{stream="audit"}=9600h'
    if [ "$got" = "$want" ]; then _ok "$ov $A loki-retention.yaml = $want"; else _fail "$ov $A loki-retention.yaml: want '$want', got '$got'"; fi
  else
    _fail "$ov ConfigMap/$A data[loki-retention.yaml] tidak ada"
  fi
done

finish
