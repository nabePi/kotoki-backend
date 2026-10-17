#!/usr/bin/env bash
# Deployment/Service/PDB/ScaledObject/ConfigMap kotoki-realtime (spec §13.10.2-13.10.5, R-22).
# Termasuk TestRealtimeDrainBudget: grace == preStop + DRAIN_SAFEPOINT_TIMEOUT + DRAIN_FLUSH_TIMEOUT + 10.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../hack/lib.sh
source "$HERE/../hack/lib.sh"

targets=("$@"); [ "${#targets[@]}" -gt 0 ] || targets=(base)
POD='.spec.template.spec'
C="$POD.containers[0]"
D=kotoki-realtime
HPA='.spec.advanced.horizontalPodAutoscalerConfig.behavior'

# secs <overlay> <Kind> <name> <yq> -> detik dari nilai "<n>s"
secs() { local v; v="$(_select "$1" "$2" "$3" "$4")" || return 1; v="${v%s}"; case "$v" in ''|*[!0-9]*) return 1;; esac; printf '%s\n' "$v"; }

for ov in "${targets[@]}"; do
  expect "$ov" Deployment $D '.metadata.labels["app.kubernetes.io/component"]' realtime
  expect "$ov" Deployment $D '.spec.replicas' 3
  expect "$ov" Deployment $D "$C.command | join(\" \")" /usr/local/bin/realtime
  expect "$ov" Deployment $D "$C.image | test(\"^ghcr.io/nabepi/kotoki-backend/realtime:\")" true
  expect "$ov" Deployment $D "$C.ports[] | select(.name == \"http\") | .containerPort" 8081
  expect "$ov" Deployment $D "$C.ports[] | select(.name == \"ops\") | .containerPort" 9090
  expect "$ov" Deployment $D "$C.resources.requests.cpu" 2
  expect "$ov" Deployment $D "$C.resources.requests.memory" 1Gi
  expect "$ov" Deployment $D "$C.resources.limits.cpu" 3
  expect "$ov" Deployment $D "$C.resources.limits.memory" 2Gi
  expect "$ov" Deployment $D "$C.env[] | select(.name == \"GOMEMLIMIT\") | .value" 1800MiB
  expect "$ov" Deployment $D "$C.env[] | select(.name == \"OPS_ADDR\") | .value" ':9090'
  expect "$ov" Deployment $D "$C.env[] | select(.name == \"HTTP_ADDR\") | .value" ':8081'
  expect "$ov" Deployment $D "$C.envFrom | map(.configMapRef.name // .secretRef.name) | join(\",\")" \
    'kotoki-common-config,kotoki-provider-config,kotoki-realtime-config,kotoki-realtime-secrets'
  expect "$ov" Deployment $D "$POD.priorityClassName" kotoki-realtime-high
  expect "$ov" Deployment $D "$POD.nodeSelector[\"kotoki.io/pool\"]" realtime
  expect "$ov" Deployment $D "$POD.tolerations | length" 1
  expect "$ov" Deployment $D "$POD.tolerations[0] | .key + \"=\" + .value + \":\" + .effect + \"/\" + .operator" 'workload=realtime:NoSchedule/Equal'

  # ---- TestRealtimeDrainBudget: 40 = 5 + 20 + 5 + 10
  expect "$ov" ConfigMap kotoki-realtime-config '.data.DRAIN_SAFEPOINT_TIMEOUT' 20s
  expect "$ov" ConfigMap kotoki-realtime-config '.data.DRAIN_FLUSH_TIMEOUT' 5s
  expect "$ov" Deployment $D "$POD.terminationGracePeriodSeconds" 40
  expect "$ov" Deployment $D "$C.lifecycle.preStop.sleep.seconds" 5
  CHECKS=$((CHECKS + 1))
  grace="$(_select "$ov" Deployment $D "$POD.terminationGracePeriodSeconds")"
  prestop="$(_select "$ov" Deployment $D "$C.lifecycle.preStop.sleep.seconds")"
  safepoint="$(secs "$ov" ConfigMap kotoki-realtime-config '.data.DRAIN_SAFEPOINT_TIMEOUT')"
  flush="$(secs "$ov" ConfigMap kotoki-realtime-config '.data.DRAIN_FLUSH_TIMEOUT')"
  if [ -z "$grace" ] || [ -z "$prestop" ] || [ -z "$safepoint" ] || [ -z "$flush" ]; then
    _fail "$ov TestRealtimeDrainBudget: nilai tidak terbaca (grace=$grace preStop=$prestop safepoint=$safepoint flush=$flush)"
  elif [ "$grace" -ne $(( prestop + safepoint + flush + 10 )) ]; then
    _fail "$ov TestRealtimeDrainBudget: grace $grace != preStop $prestop + safepoint $safepoint + flush $flush + margin 10"
  else
    _ok "$ov TestRealtimeDrainBudget: $grace = $prestop + $safepoint + $flush + 10"
  fi

  # ---- Kapasitas sesi: READY (95) <= MAX (100) <= HARD (120), nilai persis
  expect "$ov" ConfigMap kotoki-realtime-config '.data.REALTIME_READY_MAX_SESSIONS' 95
  expect "$ov" ConfigMap kotoki-realtime-config '.data.REALTIME_MAX_SESSIONS' 100
  expect "$ov" ConfigMap kotoki-realtime-config '.data.REALTIME_HARD_MAX_SESSIONS' 120
  CHECKS=$((CHECKS + 1))
  r="$(_select "$ov" ConfigMap kotoki-realtime-config '.data.REALTIME_READY_MAX_SESSIONS')"
  m="$(_select "$ov" ConfigMap kotoki-realtime-config '.data.REALTIME_MAX_SESSIONS')"
  h="$(_select "$ov" ConfigMap kotoki-realtime-config '.data.REALTIME_HARD_MAX_SESSIONS')"
  if [ -n "$r" ] && [ -n "$m" ] && [ -n "$h" ] && [ "$r" -le "$m" ] && [ "$m" -le "$h" ]; then
    _ok "$ov READY $r <= MAX $m <= HARD $h"
  else
    _fail "$ov urutan READY<=MAX<=HARD dilanggar (ready=$r max=$m hard=$h)"
  fi
  expect "$ov" ConfigMap kotoki-realtime-config '.data.HTTP_ADDR // "none"' none   # alamat di env Deployment, bukan ConfigMap
  expect "$ov" ConfigMap kotoki-realtime-config '.data.OTEL_SERVICE_NAME' kotoki-realtime

  expect "$ov" Service $D '.spec.ports[] | select(.name == "http") | .port' 8081
  expect "$ov" Service $D '.spec.ports[] | select(.name == "http") | .targetPort' http
  expect "$ov" Service $D '.spec.selector["app.kubernetes.io/component"]' realtime
  expect "$ov" Service $D '.spec.type // "ClusterIP"' ClusterIP
  expect "$ov" Service $D '.spec.sessionAffinity // "None"' None   # affinity oleh gateway (cookie kotoki_rt), bukan kube-proxy

  expect "$ov" PodDisruptionBudget $D '.spec.maxUnavailable' 1
  expect "$ov" PodDisruptionBudget $D '.spec.selector.matchLabels["app.kubernetes.io/component"]' realtime

  # ---- KEDA
  expect "$ov" ScaledObject $D '.apiVersion' keda.sh/v1alpha1
  expect "$ov" ScaledObject $D '.spec.scaleTargetRef.name' $D
  expect "$ov" ScaledObject $D '.spec.minReplicaCount' 3
  expect "$ov" ScaledObject $D '.spec.maxReplicaCount' 20
  expect "$ov" ScaledObject $D '.spec.fallback.failureThreshold' 3
  expect "$ov" ScaledObject $D '.spec.fallback.replicas' 15
  expect "$ov" ScaledObject $D '.spec.triggers | length' 2
  expect "$ov" ScaledObject $D '.spec.triggers[] | select(.type == "prometheus") | .metadata.threshold' 70
  expect "$ov" ScaledObject $D '.spec.triggers[] | select(.type == "prometheus") | .metricType' AverageValue
  expect "$ov" ScaledObject $D '.spec.triggers[] | select(.type == "prometheus") | .metadata.query' 'sum(kotoki_realtime_sessions_active{namespace="kotoki"})'
  expect "$ov" ScaledObject $D '.spec.triggers[] | select(.type == "prometheus") | .metadata.serverAddress' 'http://kube-prometheus-stack-prometheus.observability:9090'
  expect "$ov" ScaledObject $D '.spec.triggers[] | select(.type == "cpu") | .metricType' Utilization
  expect "$ov" ScaledObject $D '.spec.triggers[] | select(.type == "cpu") | .metadata.value' 70
  expect "$ov" ScaledObject $D "$HPA.scaleUp.stabilizationWindowSeconds" 0
  expect "$ov" ScaledObject $D "$HPA.scaleUp.selectPolicy" Max
  expect "$ov" ScaledObject $D "$HPA.scaleUp.policies | length" 2
  expect "$ov" ScaledObject $D "$HPA.scaleUp.policies[] | select(.type == \"Pods\") | .value" 4
  expect "$ov" ScaledObject $D "$HPA.scaleUp.policies[] | select(.type == \"Pods\") | .periodSeconds" 60
  expect "$ov" ScaledObject $D "$HPA.scaleUp.policies[] | select(.type == \"Percent\") | .value" 100
  expect "$ov" ScaledObject $D "$HPA.scaleUp.policies[] | select(.type == \"Percent\") | .periodSeconds" 60
  expect "$ov" ScaledObject $D "$HPA.scaleDown.stabilizationWindowSeconds" 600
  expect "$ov" ScaledObject $D "$HPA.scaleDown.policies | length" 1
  expect "$ov" ScaledObject $D "$HPA.scaleDown.policies[0] | .type + \":\" + (.value | tostring) + \":\" + (.periodSeconds | tostring)" 'Pods:1:300'
done

finish
