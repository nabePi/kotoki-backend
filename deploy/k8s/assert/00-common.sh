#!/usr/bin/env bash
# Invarian lintas-komponen (Global Constraints M10 / spec §13.10): berlaku untuk SEMUA Deployment
# dan seluruh ConfigMap pada tiap target yang diberikan (default: base).
# Pemakaian: 00-common.sh [target...]   (target = nama berkas out/k8s/<target>.yaml)
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../hack/lib.sh
source "$HERE/../hack/lib.sh"

targets=("$@"); [ "${#targets[@]}" -gt 0 ] || targets=(base)
POD='.spec.template.spec'
C="$POD.containers[0]"

# Daftar nama objek suatu kind; gagal keras bila kosong (tidak boleh lulus kosong).
names_of() {
  local j
  j="$(render "$1")" || return 1
  K="$2" "$YQ" -r '.[] | select(.kind == strenv(K)) | .metadata.name' "$j"
}

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
  # ---- Namespace & PriorityClass
  expect "$ov" Namespace kotoki '.metadata.labels["pod-security.kubernetes.io/enforce"]' restricted
  expect "$ov" PriorityClass kotoki-realtime-high '.value' 1000000
  expect "$ov" PriorityClass kotoki-default '.value' 1000
  expect "$ov" PriorityClass kotoki-default '.globalDefault' false

  # Hanya Deployment aplikasi (namespace kotoki); collector di namespace observability diperiksa
  # 60-observability.sh, yang juga gagal bila ada Deployment di luar dua himpunan itu.
  deps="$(K=Deployment "$YQ" -r '.[] | select(.kind == strenv(K) and .metadata.namespace == "kotoki") | .metadata.name' "$(render "$ov")")"
  if [ -z "$deps" ]; then _fail "$ov: tidak ada Deployment"; continue; fi
  for d in $deps; do
    expect "$ov" Deployment "$d" '.metadata.namespace' kotoki
    expect "$ov" Deployment "$d" '.spec.strategy.type' RollingUpdate
    expect "$ov" Deployment "$d" '.spec.strategy.rollingUpdate.maxSurge' '25%'
    expect "$ov" Deployment "$d" '.spec.strategy.rollingUpdate.maxUnavailable' 0
    # label standar + selector tidak memuat version
    expect "$ov" Deployment "$d" '.metadata.labels["app.kubernetes.io/name"]' kotoki
    expect "$ov" Deployment "$d" '.metadata.labels["app.kubernetes.io/component"] != null' true
    expect "$ov" Deployment "$d" '.metadata.labels["app.kubernetes.io/version"] != null' true
    expect "$ov" Deployment "$d" '.spec.template.metadata.labels["app.kubernetes.io/version"] != null' true
    expect "$ov" Deployment "$d" '.spec.selector.matchLabels | keys | join(",")' 'app.kubernetes.io/component,app.kubernetes.io/name'
    expect "$ov" Deployment "$d" '.spec.selector.matchLabels["app.kubernetes.io/component"] == .spec.template.metadata.labels["app.kubernetes.io/component"]' true
    # keamanan pod/container
    expect "$ov" Deployment "$d" "$POD.automountServiceAccountToken" false
    expect "$ov" Deployment "$d" "$POD.securityContext.runAsNonRoot" true
    expect "$ov" Deployment "$d" "$POD.securityContext.runAsUser" 65532
    expect "$ov" Deployment "$d" "$POD.securityContext.seccompProfile.type" RuntimeDefault
    expect "$ov" Deployment "$d" "$C.securityContext.allowPrivilegeEscalation" false
    expect "$ov" Deployment "$d" "$C.securityContext.readOnlyRootFilesystem" true
    expect "$ov" Deployment "$d" "$C.securityContext.capabilities.drop | join(\",\")" ALL
    expect "$ov" Deployment "$d" "$POD.containers | length" 1
    # /tmp emptyDir 64Mi
    expect "$ov" Deployment "$d" "$C.volumeMounts[] | select(.name == \"tmp\") | .mountPath" /tmp
    expect "$ov" Deployment "$d" "$POD.volumes[] | select(.name == \"tmp\") | .emptyDir.sizeLimit" 64Mi
    # topology spread: zone DoNotSchedule + hostname ScheduleAnyway, maxSkew 1
    # Profil dev: klaster dev (mis. kind) tidak punya label zona; DoNotSchedule pada zona membuat pod
    # Pending selamanya, jadi hanya zona yang diturunkan ke ScheduleAnyway. Overlay lain tidak berubah.
    if [ "$ov" = dev ]; then ZONE_WHEN=ScheduleAnyway; else ZONE_WHEN=DoNotSchedule; fi
    expect "$ov" Deployment "$d" "$POD.topologySpreadConstraints | map(.topologyKey + \":\" + .whenUnsatisfiable + \":\" + (.maxSkew | tostring)) | join(\",\")" \
      "topology.kubernetes.io/zone:$ZONE_WHEN:1,kubernetes.io/hostname:ScheduleAnyway:1"
    expect "$ov" Deployment "$d" "$POD.topologySpreadConstraints | map(.labelSelector.matchLabels[\"app.kubernetes.io/component\"]) | unique | length" 1
    # preStop + probes ke port ops
    expect "$ov" Deployment "$d" "$C.lifecycle.preStop.sleep.seconds" 5
    expect "$ov" Deployment "$d" "$C.ports[] | select(.name == \"ops\") | .containerPort" 9090
    expect "$ov" Deployment "$d" "$C.startupProbe.httpGet.path" /healthz
    expect "$ov" Deployment "$d" "$C.startupProbe.httpGet.port" ops
    expect "$ov" Deployment "$d" "$C.startupProbe.periodSeconds" 2
    expect "$ov" Deployment "$d" "$C.startupProbe.failureThreshold" 30
    expect "$ov" Deployment "$d" "$C.livenessProbe.httpGet.path" /healthz
    expect "$ov" Deployment "$d" "$C.livenessProbe.httpGet.port" ops
    expect "$ov" Deployment "$d" "$C.livenessProbe.periodSeconds" 10
    expect "$ov" Deployment "$d" "$C.livenessProbe.timeoutSeconds" 2
    expect "$ov" Deployment "$d" "$C.livenessProbe.failureThreshold" 3
    expect "$ov" Deployment "$d" "$C.readinessProbe.httpGet.path" /readyz
    expect "$ov" Deployment "$d" "$C.readinessProbe.httpGet.port" ops
    expect "$ov" Deployment "$d" "$C.readinessProbe.periodSeconds" 5
    expect "$ov" Deployment "$d" "$C.readinessProbe.timeoutSeconds" 2
    expect "$ov" Deployment "$d" "$C.readinessProbe.failureThreshold" 2
    # GOMEMLIMIT ada, format MiB, dan <= 90% memory limit
    expect "$ov" Deployment "$d" "$C.env[] | select(.name == \"GOMEMLIMIT\") | .value | test(\"^[0-9]+MiB\$\")" true
    # envFrom: configmap umum + tepat satu secretRef bernama kotoki-*-secrets
    expect "$ov" Deployment "$d" "$C.envFrom | map(select(.configMapRef.name == \"kotoki-common-config\")) | length" 1
    expect "$ov" Deployment "$d" "$C.envFrom | map(select(.secretRef.name | test(\"^kotoki-[a-z-]+-secrets\$\"))) | length" 1
    # priority class
    if [ "$d" = kotoki-realtime ]; then
      expect "$ov" Deployment "$d" "$POD.priorityClassName" kotoki-realtime-high
    else
      expect "$ov" Deployment "$d" "$POD.priorityClassName" kotoki-default
    fi
    # PDB maxUnavailable 1 yang menyeleksi komponen yang sama
    expect "$ov" PodDisruptionBudget "$d" '.spec.maxUnavailable' 1
    expect "$ov" PodDisruptionBudget "$d" '.spec.selector.matchLabels["app.kubernetes.io/component"] != null' true
  done

  # GOMEMLIMIT <= 90% limit memori (aritmetika di shell; yq tidak andal untuk perbandingan angka berbasis variabel)
  CHECKS=$((CHECKS + 1)); bad=""
  while read -r n g l; do
    [ -n "$n" ] || continue
    gm="${g%MiB}"
    case "$l" in *Gi) lm=$(( ${l%Gi} * 1024 ));; *Mi) lm="${l%Mi}";; *) lm=0;; esac
    if [ "$lm" -le 0 ] || [ $(( gm * 10 )) -gt $(( lm * 9 )) ]; then bad="$bad $n(GOMEMLIMIT=$g limit=$l)"; fi
  done < <("$YQ" -r '.[] | select(.kind == "Deployment") | .metadata.name + " " + (.spec.template.spec.containers[0].env[] | select(.name == "GOMEMLIMIT") | .value) + " " + .spec.template.spec.containers[0].resources.limits.memory' "$(render "$ov")")
  if [ -n "$bad" ]; then _fail "$ov GOMEMLIMIT > 90% limit memori:$bad"; else _ok "$ov GOMEMLIMIT <= 90% limit memori"; fi

  # ---- TestNoSecretsInConfigMaps: kunci sensitif dan URL berkredensial dilarang di ConfigMap
  # Kunci *_URL non-rahasia yang sah: endpoint vendor (*_BASE_URL, *_WS_BASE_URL; tanpa kredensial) dan
  # REALTIME_PUBLIC_URL. URL rahasia (DB_URL, REDIS_URL, RABBITMQ_URL, DB_MIGRATE_URL) tetap dilarang.
  _none "$ov" '.[] | select(.kind == "ConfigMap") | .metadata.name as $n | (.data // {}) | to_entries | .[]
    | select(.key | test("_URL$|_KEY$|PASSWORD$|SECRET"))
    | select(.key | test("^([A-Z]+_)?(WS_)?BASE_URL$|^REALTIME_PUBLIC_URL$") | not) | $n + "/" + .key' \
    'TestNoSecretsInConfigMaps (nama kunci)'
  _none "$ov" '.[] | select(.kind == "ConfigMap") | .metadata.name as $n | (.data // {}) | to_entries | .[]
    | select(.value | test("://[^/@ ]+:[^/@ ]+@")) | $n + "/" + .key' \
    'TestNoSecretsInConfigMaps (URL berkredensial)'
  # Secret literal hanya boleh ada di overlay dev (nilai dummy; isi diperiksa 70-overlay-dev.sh): lima
  # Secret aplikasi di namespace kotoki dan Secret dependency di kotoki-data. Selain dev: tidak ada.
  if [ "$ov" = dev ]; then
    _none "$ov" '.[] | select(.kind == "Secret") | (.metadata.namespace + "/" + .metadata.name)
      | select(test("^kotoki/kotoki-(api|realtime|worker|migrate|keda)-secrets$") == false) | select(test("^kotoki-data/") == false)' \
      'Secret hanya lima secret aplikasi dev dan dependency kotoki-data'
  else
    _none "$ov" '.[] | select(.kind == "Secret") | .metadata.name' 'tidak ada objek Secret di manifest'
  fi
  # Tidak ada pelanggaran nilai env literal (non-secretRef) bernama sensitif di Deployment
  _none "$ov" '.[] | select(.kind == "Deployment") | .metadata.name as $n | .spec.template.spec.containers[0].env[]
    | select(.name | test("_URL$|_KEY$|PASSWORD$|SECRET")) | $n + "/" + .name' \
    'tidak ada env sensitif literal di Deployment'

  # ---- ConfigMap umum
  expect "$ov" ConfigMap kotoki-common-config '.data.LOG_LEVEL' info
  expect "$ov" ConfigMap kotoki-common-config '.data.LOG_FORMAT' json
  expect "$ov" ConfigMap kotoki-common-config '.data.METRICS_ENABLED' true
  expect "$ov" ConfigMap kotoki-common-config '.data.OTEL_EXPORTER_OTLP_ENDPOINT' 'http://otel-collector.observability:4317'
  expect "$ov" ConfigMap kotoki-common-config '.data.OTEL_TRACES_SAMPLER' always_on
  expect "$ov" ConfigMap kotoki-common-config '.data.PRIVACY_POLICY_VERSION' 2026-10-01
  expect "$ov" ConfigMap kotoki-common-config '.data.RETENTION_TRANSCRIPT_DAYS' 180
  expect "$ov" ConfigMap kotoki-common-config '.data.RETENTION_SESSION_EVENTS_DAYS' 365
  expect "$ov" ConfigMap kotoki-common-config '.data.RETENTION_OUTBOX_PUBLISHED_DAYS' 7
  expect "$ov" ConfigMap kotoki-common-config '.data.RETENTION_PROCESSED_EVENTS_DAYS' 30
  expect "$ov" ConfigMap kotoki-common-config '.data.RETENTION_EXPIRED_TOKENS_DAYS' 7
  # APP_ENV milik overlay: tidak boleh ada di base; overlay wajib mengisinya dengan namanya sendiri
  if [ "$ov" = base ]; then
    _none "$ov" '.[] | select(.kind == "ConfigMap" and .metadata.name == "kotoki-common-config") | .data | keys | .[] | select(. == "APP_ENV")' \
      'kotoki-common-config tanpa APP_ENV (milik overlay)'
  else
    expect "$ov" ConfigMap kotoki-common-config '.data.APP_ENV' "$ov"
  fi

  # ---- ConfigMap provider (satu ConfigMap untuk semua workload; D-017, spec §13.2.8)
  expect "$ov" ConfigMap kotoki-provider-config '.metadata.namespace' kotoki
  expect "$ov" ConfigMap kotoki-provider-config '.metadata.name' kotoki-provider-config
  for d in kotoki-api kotoki-realtime kotoki-worker-core kotoki-worker-evaluation kotoki-worker-memory kotoki-worker-learner; do
    expect "$ov" Deployment "$d" "$C.envFrom | map(select(.configMapRef.name == \"kotoki-provider-config\")) | length" 1
  done
  expect "$ov" ConfigMap kotoki-provider-config \
    '[.data.OPENAI_STT_ENABLED, .data.ELEVENLABS_STT_ENABLED, .data.CARTESIA_STT_ENABLED] | map(select(. == "true")) | length' 1
  expect "$ov" ConfigMap kotoki-provider-config '[.data.OPENAI_LLM_ENABLED, .data.DEEPSEEK_LLM_ENABLED] | map(select(. == "true")) | length' 1
  expect "$ov" ConfigMap kotoki-provider-config '[.data.ELEVENLABS_TTS_ENABLED, .data.CARTESIA_TTS_ENABLED] | map(select(. == "true")) | length > 0' true
  expect "$ov" ConfigMap kotoki-provider-config '.data.OPENAI_EMBEDDING_ENABLED' true
  # tidak ada nama variabel warisan (D-015/D-016/D-017)
  _none "$ov" '.[] | select(.kind == "ConfigMap") | .metadata.name as $n | (.data // {}) | to_entries | .[]
    | select(.key | test("^PROVIDER_(PROFILE|TTS|STT|LLM)_|DEEPGRAM|GEMINI")) | select(.key != "PROVIDER_TTS_FALLBACK_ENABLED") | $n + "/" + .key' \
    'tanpa variabel provider warisan'
done

finish
