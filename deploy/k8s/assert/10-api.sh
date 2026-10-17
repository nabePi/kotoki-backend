#!/usr/bin/env bash
# Deployment/Service/PDB/ScaledObject/ConfigMap kotoki-api (spec §13.10.2-13.10.4).
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../hack/lib.sh
source "$HERE/../hack/lib.sh"

targets=("$@"); [ "${#targets[@]}" -gt 0 ] || targets=(base)
POD='.spec.template.spec'
C="$POD.containers[0]"
D=kotoki-api

for ov in "${targets[@]}"; do
  expect "$ov" Deployment $D '.metadata.labels["app.kubernetes.io/component"]' api
  expect "$ov" Deployment $D '.spec.replicas' 2
  expect "$ov" Deployment $D "$POD.terminationGracePeriodSeconds" 30
  expect "$ov" Deployment $D "$C.command | join(\" \")" /usr/local/bin/api
  expect "$ov" Deployment $D "$C.image | test(\"^ghcr.io/nabepi/kotoki-backend/api:\")" true
  expect "$ov" Deployment $D "$C.ports[] | select(.name == \"http\") | .containerPort" 8080
  expect "$ov" Deployment $D "$C.ports[] | select(.name == \"ops\") | .containerPort" 9090
  expect "$ov" Deployment $D "$C.resources.requests.cpu" 250m
  expect "$ov" Deployment $D "$C.resources.requests.memory" 256Mi
  expect "$ov" Deployment $D "$C.resources.limits.cpu" 1
  expect "$ov" Deployment $D "$C.resources.limits.memory" 512Mi
  expect "$ov" Deployment $D "$C.env[] | select(.name == \"GOMEMLIMIT\") | .value" 450MiB
  expect "$ov" Deployment $D "$C.envFrom | map(.configMapRef.name // .secretRef.name) | join(\",\")" \
    'kotoki-common-config,kotoki-api-config,kotoki-api-secrets'
  expect "$ov" Deployment $D "$POD.priorityClassName" kotoki-default
  expect "$ov" Deployment $D "$POD.nodeSelector[\"kotoki.io/pool\"] // \"none\"" none
  expect "$ov" Deployment $D "$POD.tolerations // \"none\"" none

  expect "$ov" ConfigMap kotoki-api-config '.data.HTTP_ADDR' ':8080'
  expect "$ov" ConfigMap kotoki-api-config '.data.OPS_ADDR' ':9090'
  expect "$ov" ConfigMap kotoki-api-config '.data.HTTP_MAX_BODY_BYTES' 65536
  expect "$ov" ConfigMap kotoki-api-config '.data.SHUTDOWN_TIMEOUT' 20s
  expect "$ov" ConfigMap kotoki-api-config '.data.OTEL_SERVICE_NAME' kotoki-api

  # grace 30 = preStop 5 + SHUTDOWN_TIMEOUT 20 + margin 5
  expect "$ov" Deployment $D "$C.lifecycle.preStop.sleep.seconds + 20 + 5 == $POD.terminationGracePeriodSeconds" true

  expect "$ov" Service $D '.spec.ports[] | select(.name == "http") | .port' 8080
  expect "$ov" Service $D '.spec.ports[] | select(.name == "http") | .targetPort' http
  expect "$ov" Service $D '.spec.selector["app.kubernetes.io/component"]' api
  expect "$ov" Service $D '.spec.type // "ClusterIP"' ClusterIP

  expect "$ov" PodDisruptionBudget $D '.spec.maxUnavailable' 1
  expect "$ov" PodDisruptionBudget $D '.spec.selector.matchLabels["app.kubernetes.io/component"]' api

  expect "$ov" ScaledObject $D '.apiVersion' keda.sh/v1alpha1
  expect "$ov" ScaledObject $D '.spec.scaleTargetRef.name' $D
  expect "$ov" ScaledObject $D '.spec.minReplicaCount' 2
  expect "$ov" ScaledObject $D '.spec.maxReplicaCount' 6
  expect "$ov" ScaledObject $D '.spec.triggers | length' 1
  expect "$ov" ScaledObject $D '.spec.triggers[0].type' cpu
  expect "$ov" ScaledObject $D '.spec.triggers[0].metricType' Utilization
  expect "$ov" ScaledObject $D '.spec.triggers[0].metadata.value' 65
  expect "$ov" ScaledObject $D '.spec.advanced.horizontalPodAutoscalerConfig.behavior.scaleDown.stabilizationWindowSeconds' 300
done

finish
