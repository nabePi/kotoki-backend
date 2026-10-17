#!/usr/bin/env bash
# Job kotoki-migrate dan kotoki-seed (spec §13.10, M10 Task 5). Keduanya hanya boleh memakai Secret
# kotoki-migrate-secrets (DB_MIGRATE_URL); kredensial runtime api/realtime/worker tidak boleh ikut.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../hack/lib.sh
source "$HERE/../hack/lib.sh"

targets=("$@"); [ "${#targets[@]}" -gt 0 ] || targets=(base)
POD='.spec.template.spec'
C="$POD.containers[0]"

for ov in "${targets[@]}"; do
  for j in migrate seed; do
    J="kotoki-$j"
    expect "$ov" Job $J '.metadata.namespace' kotoki
    expect "$ov" Job $J '.metadata.labels["app.kubernetes.io/name"]' kotoki
    expect "$ov" Job $J '.metadata.labels["app.kubernetes.io/component"]' "$j"
    expect "$ov" Job $J '.metadata.labels["app.kubernetes.io/version"] != null' true
    expect "$ov" Job $J '.spec.backoffLimit' 0
    expect "$ov" Job $J '.spec.ttlSecondsAfterFinished' 86400
    expect "$ov" Job $J '.metadata.generateName // "none"' none   # nama tetap agar kubectl wait deterministik
    expect "$ov" Job $J "$POD.restartPolicy" Never
    expect "$ov" Job $J "$POD.containers | length" 1
    expect "$ov" Job $J "$C.image | test(\"^ghcr.io/nabepi/kotoki-backend/$j[:@]\")" true
    # Secret: tepat satu envFrom, kotoki-migrate-secrets; tidak ada secret/configMap runtime lain
    expect "$ov" Job $J "$C.envFrom | length" 1
    expect "$ov" Job $J "$C.envFrom[0].secretRef.name" kotoki-migrate-secrets
    expect "$ov" Job $J "$C.env // [] | map(select(.valueFrom.secretKeyRef != null)) | length" 0
    CHECKS=$((CHECKS + 1))
    leak="$(JN="$J" "$YQ" -r '.[] | select(.kind == "Job" and .metadata.name == strenv(JN)) | [.. | select(tag == "!!str") | select(test("kotoki-(api|realtime|worker[a-z-]*)-secrets"))] | join(",")' "$(render "$ov")")" \
      || leak="yq-gagal"
    if [ -n "$leak" ]; then _fail "$ov $J memakai secret runtime: $leak"; else _ok "$ov $J tanpa secret api/realtime/worker"; fi
    # keamanan (sama dengan Deployment, kecuali probe/preStop yang tidak relevan untuk Job)
    expect "$ov" Job $J "$POD.automountServiceAccountToken" false
    expect "$ov" Job $J "$POD.securityContext.runAsNonRoot" true
    expect "$ov" Job $J "$POD.securityContext.runAsUser" 65532
    expect "$ov" Job $J "$POD.securityContext.seccompProfile.type" RuntimeDefault
    expect "$ov" Job $J "$C.securityContext.allowPrivilegeEscalation" false
    expect "$ov" Job $J "$C.securityContext.readOnlyRootFilesystem" true
    expect "$ov" Job $J "$C.securityContext.capabilities.drop | join(\",\")" ALL
    expect "$ov" Job $J "$C.volumeMounts[] | select(.name == \"tmp\") | .mountPath" /tmp
    expect "$ov" Job $J "$POD.volumes[] | select(.name == \"tmp\") | .emptyDir.sizeLimit" 64Mi
    expect "$ov" Job $J "$C.resources.limits.memory" 256Mi
  done

  # ---- migrate: args persis; $(DB_MIGRATE_URL) dikembangkan k8s dari envFrom (URL tidak ada di manifest)
  expect "$ov" Job kotoki-migrate "$C.args | length" 3
  expect "$ov" Job kotoki-migrate "$C.args[0]" -database
  expect "$ov" Job kotoki-migrate "$C.args[1]" '$(DB_MIGRATE_URL)'
  expect "$ov" Job kotoki-migrate "$C.args[2]" up
  expect "$ov" Job kotoki-migrate "$C.command // \"none\"" none   # ENTRYPOINT image: migrate -path /migrations

  # ---- seed: KOTOKI_ENV dari overlay (base: placeholder), memakai DB_MIGRATE_URL lewat envFrom
  expect "$ov" Job kotoki-seed "$C.env | map(select(.name == \"KOTOKI_ENV\")) | length" 1
  expect "$ov" Job kotoki-seed "$C.env[] | select(.name == \"KOTOKI_ENV\") | .value != \"\"" true
  expect "$ov" Job kotoki-seed "$C.args // \"none\"" none        # ENTRYPOINT image: seed.sh
  expect "$ov" Job kotoki-seed "$C.command // \"none\"" none
done

finish
