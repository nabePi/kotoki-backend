# R11 Gateway dan sertifikat

Severity: `CertificateExpiringSoon` = SEV3 selama > 7 hari tersisa, SEV1 bila sertifikat sudah kedaluwarsa atau < 48 jam; masalah gateway yang memutus WebSocket atau API = SEV1/SEV2.

Alert: CertificateExpiringSoon

## Gejala

- `CertificateExpiringSoon`: `certmanager_certificate_expiration_timestamp_seconds - time() < 14 hari` selama 1 jam (cert-manager gagal memperbarui; normalnya memperbarui jauh sebelum itu).
- Klien mendapat galat TLS, HTTP -> HTTPS tidak mengalihkan, WebSocket terputus periodik (idle atau timeout), atau satu host (`api.kotoki.id`/`realtime.kotoki.id`; staging `*.staging.kotoki.id`) tidak menjawab.

Komponen (namespace `envoy-gateway-system`): `Gateway` `kotoki` (listener `http` 80 dan `https` 443, TLS `kotoki-edge-tls`), `Certificate` `kotoki-edge` (SAN kedua host, issuer `ClusterIssuer letsencrypt-prod`), `ClientTrafficPolicy` `kotoki` (TLS >= 1.2, idle 120 detik); rute di namespace `kotoki`: `HTTPRoute` `kotoki-api`, `kotoki-realtime` (`timeouts.request: 0s`, hanya `/api/v1/realtime`), pengalihan 301, `BackendTrafficPolicy` `kotoki-realtime` (cookie consistent-hash `kotoki_rt`, TTL 3600 detik). Load balancer L4 cloud di depan Envoy wajib idle timeout >= 350 detik (bukan manifest).

## Diagnosis cepat

1. Sertifikat:

   ```bash
   kubectl -n envoy-gateway-system get certificate kotoki-edge
   kubectl -n envoy-gateway-system describe certificate kotoki-edge
   kubectl -n envoy-gateway-system get certificaterequest,order,challenge
   kubectl -n cert-manager logs deploy/cert-manager --since=1h | grep -i kotoki-edge
   kubectl -n envoy-gateway-system get secret kotoki-edge-tls -o json | jq -r '.data["tls.crt"]' | base64 -d | openssl x509 -noout -enddate -subject
   ```

   Kondisi `Ready=False`, `Order`/`Challenge` gagal (DNS-01 untuk wildcard staging, rate limit ACME, kredensial DNS), atau `ClusterIssuer letsencrypt-prod` tidak `Ready`.
2. Gateway dan listener:

   ```bash
   kubectl -n envoy-gateway-system get gateway kotoki
   kubectl -n envoy-gateway-system describe gateway kotoki        # kondisi Accepted/Programmed per listener, rujukan secret
   kubectl -n kotoki get httproute
   kubectl -n kotoki describe httproute kotoki-realtime           # Accepted/ResolvedRefs
   kubectl -n envoy-gateway-system get pods
   ```

3. Dari luar:

   ```bash
   curl -sSI https://api.<domain>/api/v1/auth/terms        # 200 + Strict-Transport-Security
   curl -sSI http://api.<domain>/api/v1/auth/terms         # 301 ke https
   openssl s_client -connect realtime.<domain>:443 -servername realtime.<domain> </dev/null 2>/dev/null | openssl x509 -noout -dates -issuer
   ```

4. WebSocket putus berkala: cocokkan interval putus dengan 120 detik (idle Envoy), 350 detik (L4) atau 3600 detik; periksa `kotoki_realtime_ws_close_total` dan `sum by (result) (rate(kotoki_realtime_connections_total[5m]))`.

## Mitigasi

- Sertifikat gagal diperbarui: perbaiki penyebab pada `Order`/`Challenge` (issuer, solver DNS, kuota ACME), lalu paksa penerbitan ulang:

  ```bash
  kubectl cert-manager renew kotoki-edge -n envoy-gateway-system      # bila plugin cmctl/kubectl-cert_manager terpasang
  # atau hapus Secret agar cert-manager menerbitkan ulang (jendela singkat tanpa sertifikat valid):
  kubectl -n envoy-gateway-system delete secret kotoki-edge-tls
  ```

  Gateway menyajikan sertifikat baru begitu Secret terbit; pantau `describe certificate` sampai `Ready=True`.
- Sertifikat sudah kedaluwarsa dan penerbitan tak kunjung berhasil: pasang sertifikat sementara yang valid ke Secret `kotoki-edge-tls` (diterbitkan manual oleh pemilik domain), catat sebagai SEV1, dan perbaiki penerbitan otomatis sesudahnya.
- Listener/route tidak `Accepted`: perbaiki rujukan (nama Secret `kotoki-edge-tls`, `sectionName`, label namespace `kubernetes.io/metadata.name: kotoki` pada `allowedRoutes`), lalu `kubectl apply -k deploy/k8s/overlays/<env>` melalui pipeline deploy (jangan mengubah manifest hidup tanpa menyamakan sumbernya).
- WebSocket terputus karena timeout: pastikan `timeouts.request: 0s` pada rute realtime, idle 120 detik (`ClientTrafficPolicy`/`BackendTrafficPolicy`) dan L4 >= 350 detik; ping WS server 15 detik menjaga koneksi aktif.
- Rollback konfigurasi gateway: `git revert` lalu deploy ulang overlay; hindari edit langsung di klaster.

## Verifikasi pulih

- `kubectl -n envoy-gateway-system get certificate kotoki-edge` -> `READY True`; tanggal `notAfter` baru (> 60 hari); `CertificateExpiringSoon` padam setelah metrik diperbarui.
- `curl -sSI` HTTPS menjawab dengan HSTS (`max-age=31536000; includeSubDomains`) dan HTTP mengalihkan 301; `openssl s_client` menampilkan rantai valid.
- Satu percakapan `kotoki-cli converse` lewat `wss://realtime.<domain>/api/v1/realtime` berhasil; tidak ada kenaikan `rejected_*` pada `kotoki_realtime_connections_total`.
