#!/usr/bin/env bash
# Deployment/PDB/ConfigMap worker-{core,evaluation,memory,learner} + KEDA RabbitMQ (spec §13.10.2-13.10.3, §9.8).
# Grace pod == WORKER_SHUTDOWN_GRACE + 10 dihitung dari ConfigMap hasil render (bukan konstanta).
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../hack/lib.sh
source "$HERE/../hack/lib.sh"

targets=("$@"); [ "${#targets[@]}" -gt 0 ] || targets=(base)
POD='.spec.template.spec'
C="$POD.containers[0]"

secs() { local v; v="$(_select "$1" "$2" "$3" "$4")" || return 1; v="${v%s}"; case "$v" in ''|*[!0-9]*) return 1;; esac; printf '%s\n' "$v"; }

# name|args|replicas|req cpu|req mem|lim cpu|lim mem|GOMEMLIMIT|grace|WORKER_SHUTDOWN_GRACE
WORKERS='core|--handlers=outbox,sweeper,cleanup|2|100m|128Mi|500m|256Mi|220MiB|35|25s
evaluation|--handlers=evaluation|2|250m|256Mi|1|512Mi|450MiB|210|200s
memory|--handlers=memory|1|250m|256Mi|1|512Mi|450MiB|130|120s
learner|--handlers=learner,summary|1|100m|128Mi|500m|256Mi|220MiB|70|60s'

for ov in "${targets[@]}"; do
  while IFS='|' read -r w args rep rcpu rmem lcpu lmem gomem grace shut; do
    D="kotoki-worker-$w"; CM="kotoki-worker-$w-config"
    expect "$ov" Deployment $D '.metadata.labels["app.kubernetes.io/component"]' "worker-$w"
    expect "$ov" Deployment $D '.spec.replicas' "$rep"
    expect "$ov" Deployment $D "$C.command | join(\" \")" /usr/local/bin/worker
    # args[0] (bukan join): yq v4.45.1 memecah elemen berkoma pada join
    expect "$ov" Deployment $D "$C.args | length" 1
    expect "$ov" Deployment $D "$C.args[0]" "$args"
    expect "$ov" Deployment $D "$C.image | test(\"^ghcr.io/nabepi/kotoki-backend/worker:\")" true
    expect "$ov" Deployment $D "$C.ports | length" 1
    expect "$ov" Deployment $D "$C.resources.requests.cpu" "$rcpu"
    expect "$ov" Deployment $D "$C.resources.requests.memory" "$rmem"
    expect "$ov" Deployment $D "$C.resources.limits.cpu" "$lcpu"
    expect "$ov" Deployment $D "$C.resources.limits.memory" "$lmem"
    expect "$ov" Deployment $D "$C.env[] | select(.name == \"GOMEMLIMIT\") | .value" "$gomem"
    # port ops 9090 (spec §13.10.2); default config.LoadWorker adalah :8081 sehingga wajib eksplisit
    expect "$ov" Deployment $D "$C.env[] | select(.name == \"WORKER_HTTP_ADDR\") | .value" ':9090'
    expect "$ov" Deployment $D "$C.env | map(select(.name == \"HTTP_ADDR\" or .name == \"OPS_ADDR\")) | length" 0
    expect "$ov" Deployment $D "$C.envFrom | map(.configMapRef.name // .secretRef.name) | join(\",\")" \
      "kotoki-common-config,$CM,kotoki-worker-secrets"
    expect "$ov" Deployment $D "$POD.priorityClassName" kotoki-default
    expect "$ov" Deployment $D "$POD.nodeSelector[\"kotoki.io/pool\"] // \"none\"" none
    expect "$ov" Deployment $D "$POD.tolerations // \"none\"" none

    # ---- grace == WORKER_SHUTDOWN_GRACE + 10
    expect "$ov" ConfigMap "$CM" '.data.WORKER_SHUTDOWN_GRACE' "$shut"
    expect "$ov" ConfigMap "$CM" '.data.OTEL_SERVICE_NAME' "$D"
    expect "$ov" Deployment $D "$POD.terminationGracePeriodSeconds" "$grace"
    CHECKS=$((CHECKS + 1))
    g="$(_select "$ov" Deployment $D "$POD.terminationGracePeriodSeconds")"
    s="$(secs "$ov" ConfigMap "$CM" '.data.WORKER_SHUTDOWN_GRACE')"
    if [ -z "$g" ] || [ -z "$s" ]; then
      _fail "$ov $D grace: nilai tidak terbaca (grace=$g shutdown=$s)"
    elif [ "$g" -ne $(( s + 10 )) ]; then
      _fail "$ov $D grace $g != WORKER_SHUTDOWN_GRACE $s + 10"
    else
      _ok "$ov $D grace $g = WORKER_SHUTDOWN_GRACE $s + 10"
    fi
    expect "$ov" ConfigMap "$CM" '.data.WORKER_HTTP_ADDR // "none"' none   # alamat di env Deployment, bukan ConfigMap
    expect "$ov" ConfigMap "$CM" '.data.WORKER_HANDLERS // "none"' none    # handler hanya lewat args

    expect "$ov" PodDisruptionBudget $D '.spec.maxUnavailable' 1
    expect "$ov" PodDisruptionBudget $D '.spec.selector.matchLabels["app.kubernetes.io/component"]' "worker-$w"
  done <<<"$WORKERS"

  # ---- handler tidak tumpang tindih: tiap nama handler §9.8 tepat satu kali di seluruh worker
  for h in outbox sweeper cleanup evaluation memory learner summary; do
    CHECKS=$((CHECKS + 1))
    j="$(render "$ov")" || { _fail "$ov handler $h: render gagal"; continue; }
    n="$(H="$h" "$YQ" -r '[.[] | select(.kind == "Deployment" and (.metadata.name | test("^kotoki-worker-"))) | .spec.template.spec.containers[0].args[] | sub("^--handlers=";"") | split(",") | .[] | select(. == strenv(H))] | length' "$j")"
    if [ "$n" = 1 ]; then _ok "$ov handler $h tepat satu worker"; else _fail "$ov handler $h muncul $n kali (harus 1)"; fi
  done
  # tidak ada Deployment worker lain di luar empat yang dikenal
  expect "$ov" Deployment kotoki-worker-core '.metadata.name' kotoki-worker-core

  # ---- KEDA: TriggerAuthentication
  expect "$ov" TriggerAuthentication kotoki-keda-rabbitmq '.apiVersion' keda.sh/v1alpha1
  expect "$ov" TriggerAuthentication kotoki-keda-rabbitmq '.spec.secretTargetRef | length' 1
  expect "$ov" TriggerAuthentication kotoki-keda-rabbitmq '.spec.secretTargetRef[0] | .parameter + ":" + .name + ":" + .key' 'host:kotoki-keda-secrets:host'

  # ---- KEDA: ScaledObject RabbitMQ  name|min|max|queue|value
  while IFS='|' read -r w min max q val; do
    D="kotoki-worker-$w"
    expect "$ov" ScaledObject $D '.apiVersion' keda.sh/v1alpha1
    expect "$ov" ScaledObject $D '.spec.scaleTargetRef.name' $D
    expect "$ov" ScaledObject $D '.spec.minReplicaCount' "$min"
    expect "$ov" ScaledObject $D '.spec.maxReplicaCount' "$max"
    expect "$ov" ScaledObject $D '.spec.triggers | length' 1
    expect "$ov" ScaledObject $D '.spec.triggers[0].type' rabbitmq
    expect "$ov" ScaledObject $D '.spec.triggers[0].metadata.mode' QueueLength
    expect "$ov" ScaledObject $D '.spec.triggers[0].metadata.value' "$val"
    expect "$ov" ScaledObject $D '.spec.triggers[0].metadata.queueName' "$q"
    expect "$ov" ScaledObject $D '.spec.triggers[0].metadata.vhostName' /kotoki
    expect "$ov" ScaledObject $D '.spec.triggers[0].metadata.protocol' amqp
    expect "$ov" ScaledObject $D '.spec.triggers[0].authenticationRef.name' kotoki-keda-rabbitmq
    # kredensial tidak boleh literal di trigger
    expect "$ov" ScaledObject $D '.spec.triggers[0].metadata.host // "none"' none
  done <<'EOS'
evaluation|2|8|evaluation.session_completed|5
memory|1|4|memory.session_completed|10
learner|1|3|learner.evaluation_completed|20
EOS
  expect_absent "$ov" ScaledObject kotoki-worker-core
done

finish
