#!/usr/bin/env bash
# Gateway, route, policy, TLS dan NetworkPolicy (spec §13.10.6-13.10.7, M10 Task 6).
# Ekuivalen TestGatewayRealtimeRoute: WS harus bertahan lewat gateway (timeout request 0s, idle 120s,
# cookie affinity kotoki_rt TTL 3600s, tanpa max connection duration, TLS >= 1.2, HTTP -> 301, HSTS).
# Persyaratan di luar manifest: L4 load balancer di depan Envoy memakai TCP idle timeout >= 350 s.
# Pemakaian: 50-edge.sh [target...]   (default: base; staging memakai host *.staging.kotoki.id)
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../hack/lib.sh
source "$HERE/../hack/lib.sh"

targets=("$@"); [ "${#targets[@]}" -gt 0 ] || targets=(base)
GWNS=envoy-gateway-system
HSTS='max-age=31536000; includeSubDomains'
COMP='app.kubernetes.io/component'

# _np_select <komponen>: ekspresi yq yang memilih semua NetworkPolicy yang mengenai pod komponen itu
# (podSelector {} = semua pod, matchLabels, atau matchExpressions In).
_np_select() {
  printf '[.[] | select(.kind == "NetworkPolicy") | select((.spec.podSelector | length) == 0
    or .spec.podSelector.matchLabels["%s"] == "%s"
    or ((.spec.podSelector.matchExpressions // []) | map(select(.key == "%s" and .operator == "In" and ((.values // []) | map(select(. == "%s")) | length) > 0)) | length) > 0)]' \
    "$COMP" "$1" "$COMP" "$1"
}

# np_ports <ov> <komponen> <egress|ingress> <label> <want>: gabungan port semua aturan. Aturan tanpa
# `ports` (= semua port) dihitung sebagai pelanggaran, bukan dilewati.
np_ports() {
  local ov="$1" comp="$2" dir="$3" label="$4" want="$5" j got
  CHECKS=$((CHECKS + 1))
  j="$(render "$ov")" || { _fail "$ov $label: render gagal"; return 1; }
  got="$("$YQ" -r "$(_np_select "$comp") | [.[].spec.$dir // [] | .[] | select((.ports // []) | length == 0)] | length" "$j")" || { _fail "$ov $label: yq gagal"; return 1; }
  if [ "$got" != 0 ]; then _fail "$ov $label: $got aturan $dir tanpa ports (terbuka semua port)"; return 1; fi
  got="$("$YQ" -r "$(_np_select "$comp") | [.[].spec.$dir // [] | .[] | .ports[].port] | unique | sort | map(tostring) | join(\",\")" "$j")" || { _fail "$ov $label: yq gagal"; return 1; }
  if [ "$got" != "$want" ]; then _fail "$ov $label: want '$want', got '$got'"; return 1; fi
  _ok "$ov $label = $want"
}

# np_rule_sources <ov> <komponen> <ingress|egress> <port> <label> <want>: peer unik (namespace atau ipBlock)
# dari aturan yang memuat <port>.
np_peers() {
  local ov="$1" comp="$2" dir="$3" port="$4" label="$5" want="$6" j got peer
  [ "$dir" = ingress ] && peer=from || peer=to
  CHECKS=$((CHECKS + 1))
  j="$(render "$ov")" || { _fail "$ov $label: render gagal"; return 1; }
  got="$(P="$port" "$YQ" -r "$(_np_select "$comp") | [.[].spec.$dir // [] | .[] | select((.ports // []) | map(select(.port == (env(P) | tonumber))) | length > 0) | (.$peer // [\"OPEN\"])[] | (.namespaceSelector.matchLabels[\"kubernetes.io/metadata.name\"] // .ipBlock.cidr // \"OTHER\")] | unique | sort | join(\",\")" "$j")" || { _fail "$ov $label: yq gagal"; return 1; }
  if [ "$got" != "$want" ]; then _fail "$ov $label: want '$want', got '$got'"; return 1; fi
  _ok "$ov $label = $want"
}

for ov in "${targets[@]}"; do
  case "$ov" in
    dev)     API_HOST=api.dev.kotoki.id;     RT_HOST=realtime.dev.kotoki.id;     SAN='*.dev.kotoki.id'     ;;
    staging) API_HOST=api.staging.kotoki.id; RT_HOST=realtime.staging.kotoki.id; SAN='*.staging.kotoki.id' ;;
    *)       API_HOST=api.kotoki.id;         RT_HOST=realtime.kotoki.id;         SAN=''                    ;;
  esac
  # dev: RabbitMQ in-cluster tanpa TLS, sehingga worker juga boleh egress 5672 ke kotoki-data.
  if [ "$ov" = dev ]; then WORKER_PORTS='53,443,4317,5432,5671,5672,6379,6380'; else WORKER_PORTS='53,443,4317,5432,5671,6379,6380'; fi

  # ---- Gateway: satu gateway, listener 80 (redirect) dan 443 (TLS terminate, satu secret SAN)
  expect "$ov" Gateway kotoki '.metadata.namespace' $GWNS
  expect "$ov" Gateway kotoki '.spec.gatewayClassName' envoy-gateway
  expect "$ov" Gateway kotoki '.spec.listeners | length' 2
  expect "$ov" Gateway kotoki '.spec.listeners[] | select(.name == "http") | .port' 80
  expect "$ov" Gateway kotoki '.spec.listeners[] | select(.name == "http") | .protocol' HTTP
  expect "$ov" Gateway kotoki '.spec.listeners[] | select(.name == "https") | .port' 443
  expect "$ov" Gateway kotoki '.spec.listeners[] | select(.name == "https") | .protocol' HTTPS
  expect "$ov" Gateway kotoki '.spec.listeners[] | select(.name == "https") | .tls.mode' Terminate
  expect "$ov" Gateway kotoki '.spec.listeners[] | select(.name == "https") | .tls.certificateRefs[0].name' kotoki-edge-tls
  expect "$ov" Gateway kotoki '.spec.listeners[] | select(.name == "https") | .tls.certificateRefs | length' 1
  # route hanya dari namespace kotoki
  expect "$ov" Gateway kotoki '.spec.listeners[] | select(.name == "https") | .allowedRoutes.namespaces.from' Selector
  expect "$ov" Gateway kotoki '.spec.listeners[] | select(.name == "https") | .allowedRoutes.namespaces.selector.matchLabels["kubernetes.io/metadata.name"]' kotoki
  expect "$ov" Gateway kotoki '.spec.listeners[] | select(.name == "http") | .allowedRoutes.namespaces.selector.matchLabels["kubernetes.io/metadata.name"]' kotoki

  # ---- HTTPRoute realtime: host terpisah, hanya /api/v1/realtime, timeout request 0s
  R=kotoki-realtime
  expect "$ov" HTTPRoute $R '.metadata.namespace' kotoki
  expect "$ov" HTTPRoute $R '.spec.hostnames | join(",")' "$RT_HOST"
  expect "$ov" HTTPRoute $R '.spec.parentRefs[0].name' kotoki
  expect "$ov" HTTPRoute $R '.spec.parentRefs[0].namespace' $GWNS
  expect "$ov" HTTPRoute $R '.spec.parentRefs[0].sectionName' https
  expect "$ov" HTTPRoute $R '.spec.parentRefs | length' 1
  expect "$ov" HTTPRoute $R '.spec.rules | length' 1
  expect "$ov" HTTPRoute $R '.spec.rules[0].matches | length' 1
  expect "$ov" HTTPRoute $R '.spec.rules[0].matches[0].path.type' PathPrefix
  expect "$ov" HTTPRoute $R '.spec.rules[0].matches[0].path.value' /api/v1/realtime
  expect "$ov" HTTPRoute $R '.spec.rules[0].timeouts.request' 0s
  expect "$ov" HTTPRoute $R '.spec.rules[0].timeouts.backendRequest // "none"' none
  expect "$ov" HTTPRoute $R '.spec.rules[0].backendRefs | length' 1
  expect "$ov" HTTPRoute $R '.spec.rules[0].backendRefs[0].name' kotoki-realtime
  expect "$ov" HTTPRoute $R '.spec.rules[0].backendRefs[0].port' 8081
  expect "$ov" HTTPRoute $R '.spec.rules[0].filters[] | select(.type == "ResponseHeaderModifier") | .responseHeaderModifier.set[] | select(.name == "Strict-Transport-Security") | .value' "$HSTS"

  # ---- HTTPRoute api: /api/v1 saja; /api/v1/realtime tidak dirutekan di host ini
  A=kotoki-api
  expect "$ov" HTTPRoute $A '.metadata.namespace' kotoki
  expect "$ov" HTTPRoute $A '.spec.hostnames | join(",")' "$API_HOST"
  expect "$ov" HTTPRoute $A '.spec.parentRefs[0].name' kotoki
  expect "$ov" HTTPRoute $A '.spec.parentRefs[0].namespace' $GWNS
  expect "$ov" HTTPRoute $A '.spec.parentRefs[0].sectionName' https
  expect "$ov" HTTPRoute $A '.spec.parentRefs | length' 1
  expect "$ov" HTTPRoute $A '.spec.rules | length' 1
  expect "$ov" HTTPRoute $A '.spec.rules[0].matches | length' 1
  expect "$ov" HTTPRoute $A '.spec.rules[0].matches[0].path.type' PathPrefix
  expect "$ov" HTTPRoute $A '.spec.rules[0].matches[0].path.value' /api/v1
  expect "$ov" HTTPRoute $A '[.spec.rules[].matches[].path.value] | map(select(test("realtime"))) | length' 0
  expect "$ov" HTTPRoute $A '.spec.rules[0].timeouts.request' 30s
  expect "$ov" HTTPRoute $A '.spec.rules[0].backendRefs | length' 1
  expect "$ov" HTTPRoute $A '.spec.rules[0].backendRefs[0].name' kotoki-api
  expect "$ov" HTTPRoute $A '.spec.rules[0].backendRefs[0].port' 8080
  expect "$ov" HTTPRoute $A '.spec.rules[0].filters[] | select(.type == "ResponseHeaderModifier") | .responseHeaderModifier.set[] | select(.name == "Strict-Transport-Security") | .value' "$HSTS"

  # ---- HTTP -> HTTPS 301 (listener 80, semua host)
  X=kotoki-redirect
  expect "$ov" HTTPRoute $X '.metadata.namespace' kotoki
  expect "$ov" HTTPRoute $X '.spec.parentRefs[0].name' kotoki
  expect "$ov" HTTPRoute $X '.spec.parentRefs[0].namespace' $GWNS
  expect "$ov" HTTPRoute $X '.spec.parentRefs[0].sectionName' http
  expect "$ov" HTTPRoute $X '.spec.rules[0].filters[0].type' RequestRedirect
  expect "$ov" HTTPRoute $X '.spec.rules[0].filters[0].requestRedirect.scheme' https
  expect "$ov" HTTPRoute $X '.spec.rules[0].filters[0].requestRedirect.statusCode' 301
  expect "$ov" HTTPRoute $X '.spec.rules[0].backendRefs // "none"' none

  # ---- BackendTrafficPolicy realtime: consistent-hash cookie, idle 120s, tanpa max connection duration
  P=kotoki-realtime
  expect "$ov" BackendTrafficPolicy $P '.metadata.namespace' kotoki
  expect "$ov" BackendTrafficPolicy $P '.spec.targetRefs | length' 1
  expect "$ov" BackendTrafficPolicy $P '.spec.targetRefs[0].kind' HTTPRoute
  expect "$ov" BackendTrafficPolicy $P '.spec.targetRefs[0].name' kotoki-realtime
  expect "$ov" BackendTrafficPolicy $P '.spec.loadBalancer.type' ConsistentHash
  expect "$ov" BackendTrafficPolicy $P '.spec.loadBalancer.consistentHash.type' Cookie
  expect "$ov" BackendTrafficPolicy $P '.spec.loadBalancer.consistentHash.cookie.name' kotoki_rt
  expect "$ov" BackendTrafficPolicy $P '.spec.loadBalancer.consistentHash.cookie.ttl' 3600s
  expect "$ov" BackendTrafficPolicy $P '.spec.loadBalancer.consistentHash.cookie.attributes.Path' /api/v1/realtime
  expect "$ov" BackendTrafficPolicy $P '.spec.timeout.http.connectionIdleTimeout' 120s
  expect "$ov" BackendTrafficPolicy $P '.spec.timeout.http.maxConnectionDuration // "none"' none
  expect "$ov" BackendTrafficPolicy $P '.spec.timeout.http.requestTimeout // "none"' none   # batas request hanya dari HTTPRoute (0s)

  # ---- BackendTrafficPolicy api: round robin, idle 120s
  P=kotoki-api
  expect "$ov" BackendTrafficPolicy $P '.metadata.namespace' kotoki
  expect "$ov" BackendTrafficPolicy $P '.spec.targetRefs | length' 1
  expect "$ov" BackendTrafficPolicy $P '.spec.targetRefs[0].kind' HTTPRoute
  expect "$ov" BackendTrafficPolicy $P '.spec.targetRefs[0].name' kotoki-api
  expect "$ov" BackendTrafficPolicy $P '.spec.loadBalancer.type' RoundRobin
  expect "$ov" BackendTrafficPolicy $P '.spec.timeout.http.connectionIdleTimeout' 120s
  expect "$ov" BackendTrafficPolicy $P '.spec.timeout.http.maxConnectionDuration // "none"' none

  # ---- ClientTrafficPolicy: TLS >= 1.2, HTTP/1.1 diizinkan (WS upgrade), idle 120s
  expect "$ov" ClientTrafficPolicy kotoki '.metadata.namespace' $GWNS
  expect "$ov" ClientTrafficPolicy kotoki '.spec.targetRefs[0].kind' Gateway
  expect "$ov" ClientTrafficPolicy kotoki '.spec.targetRefs[0].name' kotoki
  expect "$ov" ClientTrafficPolicy kotoki '.spec.tls.minVersion' '1.2'
  expect "$ov" ClientTrafficPolicy kotoki '.spec.tls.alpnProtocols | join(",")' 'h2,http/1.1'
  expect "$ov" ClientTrafficPolicy kotoki '.spec.timeout.http.idleTimeout' 120s
  expect "$ov" ClientTrafficPolicy kotoki '.spec.http1.http10 // "none"' none

  # ---- Certificate: satu secret, SAN kedua host (staging: wildcard), issuer letsencrypt-prod
  expect "$ov" Certificate kotoki-edge '.metadata.namespace' $GWNS
  expect "$ov" Certificate kotoki-edge '.spec.secretName' kotoki-edge-tls
  expect "$ov" Certificate kotoki-edge '.spec.issuerRef.name' letsencrypt-prod
  expect "$ov" Certificate kotoki-edge '.spec.issuerRef.kind' ClusterIssuer
  if [ -n "$SAN" ]; then
    expect "$ov" Certificate kotoki-edge '.spec.dnsNames | join(",")' "$SAN"
  else
    expect "$ov" Certificate kotoki-edge '.spec.dnsNames | map(select(. == "api.kotoki.id")) | length' 1
    expect "$ov" Certificate kotoki-edge '.spec.dnsNames | map(select(. == "realtime.kotoki.id")) | length' 1
    expect "$ov" Certificate kotoki-edge '.spec.dnsNames | length' 2
  fi
  expect "$ov" Certificate kotoki-edge '.spec.privateKey.rotationPolicy' Always
  expect_absent "$ov" Secret kotoki-edge-tls     # secret TLS dibuat cert-manager, tidak pernah ada di manifest

  # ---- NetworkPolicy: default-deny ingress+egress seluruh namespace
  expect "$ov" NetworkPolicy kotoki-default-deny '.metadata.namespace' kotoki
  expect "$ov" NetworkPolicy kotoki-default-deny '.spec.podSelector | length' 0
  expect "$ov" NetworkPolicy kotoki-default-deny '.spec.policyTypes | join(",")' Ingress,Egress
  expect "$ov" NetworkPolicy kotoki-default-deny '.spec.ingress // "none"' none
  expect "$ov" NetworkPolicy kotoki-default-deny '.spec.egress // "none"' none
  CHECKS=$((CHECKS + 1))
  bad="$("$YQ" -r '[.[] | select(.kind == "NetworkPolicy") | select(.metadata.namespace != "kotoki") | .metadata.name] | join(",")' "$(render "$ov")")" || bad=yq-gagal
  if [ -n "$bad" ]; then _fail "$ov NetworkPolicy di luar namespace kotoki: $bad"; else _ok "$ov semua NetworkPolicy di namespace kotoki"; fi

  # ---- semua pod: DNS, scrape Prometheus 9090, OTLP 4317
  np_ports "$ov" api       egress  "api egress"      '53,587,4317,5432,6379,6380'
  np_ports "$ov" realtime  egress  "realtime egress" '53,443,4317,5432,6379,6380'
  for w in worker-core worker-evaluation worker-memory worker-learner; do
    np_ports "$ov" $w egress "$w egress" "$WORKER_PORTS"
  done
  # Job migrate/seed: hanya DNS + PostgreSQL
  np_ports "$ov" migrate egress "migrate egress" '53,4317,5432'
  np_ports "$ov" seed    egress "seed egress"    '53,4317,5432'

  # api tanpa egress 443 dan RabbitMQ (eksplisit, di luar kesamaan himpunan di atas)
  CHECKS=$((CHECKS + 1))
  bad="$("$YQ" -r "$(_np_select api) | [.[].spec.egress // [] | .[] | .ports[].port | select(. == 443 or . == 5671 or . == 5672)] | length" "$(render "$ov")")" || bad=yq-gagal
  if [ "$bad" != 0 ]; then _fail "$ov api punya egress 443/RabbitMQ"; else _ok "$ov api tanpa egress 443/RabbitMQ"; fi

  # ingress
  np_ports "$ov" api      ingress "api ingress"      '8080,9090'
  np_ports "$ov" realtime ingress "realtime ingress" '8081,9090'
  np_ports "$ov" worker-core ingress "worker-core ingress" '9090'
  np_peers "$ov" api      ingress 8080 "api 8080 hanya dari gateway"      $GWNS
  np_peers "$ov" realtime ingress 8081 "realtime 8081 hanya dari gateway" $GWNS
  for c in api realtime worker-core worker-evaluation worker-memory worker-learner; do
    np_peers "$ov" $c ingress 9090 "$c 9090 hanya dari observability" observability
  done

  # egress: tujuan
  np_peers "$ov" api      egress 4317 "api OTLP ke observability"      observability
  np_peers "$ov" realtime egress 4317 "realtime OTLP ke observability" observability
  np_peers "$ov" worker-core egress 4317 "worker OTLP ke observability" observability
  np_peers "$ov" worker-core egress 5671 "worker RabbitMQ ke kotoki-data" kotoki-data
  np_peers "$ov" api      egress 587 "api SMTP ke internet (ipBlock)" 0.0.0.0/0
  np_peers "$ov" realtime egress 443 "realtime 443 ke internet (ipBlock)" 0.0.0.0/0
  np_peers "$ov" worker-evaluation egress 443 "worker 443 ke internet (ipBlock)" 0.0.0.0/0
  # data store lewat ipBlock (CIDR dari overlay; base = TEST-NET placeholder); tidak ada namespaceSelector.
  # dev: dependency in-cluster di kotoki-data (namespaceSelector), RabbitMQ 5672 juga ke kotoki-data.
  if [ "$ov" = base ]; then DS_PEER=192.0.2.0/24; elif [ "$ov" = dev ]; then DS_PEER=kotoki-data; else DS_PEER=''; fi
  if [ -n "$DS_PEER" ]; then
    for c in api realtime worker-core migrate seed; do
      np_peers "$ov" $c egress 5432 "$c PostgreSQL data store" "$DS_PEER"
    done
    for c in api realtime worker-core; do
      np_peers "$ov" $c egress 6379 "$c Redis data store" "$DS_PEER"
      np_peers "$ov" $c egress 6380 "$c Redis TLS data store" "$DS_PEER"
    done
  fi
  if [ "$ov" = dev ]; then np_peers "$ov" worker-core egress 5672 "worker RabbitMQ dev (5672) ke kotoki-data" kotoki-data; fi
  # ipBlock internet mengecualikan RFC1918 (443 dan 587)
  CHECKS=$((CHECKS + 1))
  bad="$("$YQ" -r '[.[] | select(.kind == "NetworkPolicy") | .spec.egress // [] | .[] | select((.ports // []) | map(select(.port == 443 or .port == 587)) | length > 0) | .to[] | select(.ipBlock.cidr == "0.0.0.0/0") | select(((.ipBlock.except // []) | sort | join(",")) != "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16")] | length' "$(render "$ov")")" || bad=yq-gagal
  if [ "$bad" != 0 ]; then _fail "$ov $bad aturan internet 443/587 tidak mengecualikan RFC1918"; else _ok "$ov internet 443/587 kecuali RFC1918"; fi
  # tidak ada peer 0.0.0.0/0 tanpa except di aturan egress manapun
  CHECKS=$((CHECKS + 1))
  bad="$("$YQ" -r '[.[] | select(.kind == "NetworkPolicy") | .spec.egress // [] | .[] | (.to // [])[] | select(.ipBlock.cidr == "0.0.0.0/0") | select((.ipBlock.except // []) | length == 0)] | length' "$(render "$ov")")" || bad=yq-gagal
  if [ "$bad" != 0 ]; then _fail "$ov $bad aturan egress 0.0.0.0/0 tanpa except"; else _ok "$ov tidak ada egress 0.0.0.0/0 telanjang"; fi
done

finish
