# R6 Kapasitas realtime, slow consumer dan pod crash

Severity: `RealtimeRejectingCapacity` = SEV1 (pengguna ditolak masuk); `RealtimeCapacityHigh`, `RealtimeOutboundDrops`, `PodCrashLooping` = SEV2/SEV3 (crash realtime berulang menaikkannya ke SEV1).

Alert: RealtimeCapacityHigh
Alert: RealtimeRejectingCapacity
Alert: RealtimeOutboundDrops
Alert: PodCrashLooping

## Gejala

- `RealtimeCapacityHigh`: `sum(kotoki_realtime_sessions_active) / sum(kotoki_realtime_session_capacity)` > 80% selama 5 menit.
- `RealtimeRejectingCapacity`: ada laju `kotoki_realtime_connections_total{result="rejected_capacity"}` selama 5 menit; pod penuh (batas `REALTIME_MAX_SESSIONS` 100, batas keras `REALTIME_HARD_MAX_SESSIONS` 120; readiness berhenti di `REALTIME_READY_MAX_SESSIONS` 95).
- `RealtimeOutboundDrops`: laju `kotoki_realtime_slow_consumer_total` > 0,1/detik selama 10 menit; koneksi diputus karena antrean keluar penuh (klien lambat atau jaringan buruk).
- `PodCrashLooping`: lebih dari 3 restart dalam 15 menit pada pod di namespace `kotoki` atau `observability`.

## Diagnosis cepat

1. Dashboard `Kotoki Overview` ("Realtime capacity used", "Active sessions vs capacity", "Draining realtime pods") dan `Realtime Latency` ("Sessions per pod by state", "Outbound queue depth P95 per queue", "Slow consumers and dropped transient frames", "Realtime CPU/memory per pod").
2. Status autoscaling (KEDA memiliki HPA; target 70 sesi per pod, min 3, max 20, fallback 15 replika bila Prometheus tak terjangkau):

   ```bash
   kubectl -n kotoki get scaledobject kotoki-realtime
   kubectl -n kotoki describe scaledobject kotoki-realtime | tail -30
   kubectl -n kotoki get hpa
   kubectl -n kotoki get pods -l app.kubernetes.io/component=realtime -o wide
   ```

   Pod `Pending`: node pool `kotoki.io/pool: realtime` penuh atau tidak punya toleration/zona (`kubectl -n kotoki describe pod <pod>`).
3. Prometheus terjangkau dari KEDA? Kondisi `Ready`/`Active`/`Fallback` ScaledObject dan log operator: `kubectl -n keda logs deploy/keda-operator --since=15m`.
4. Crash: `kubectl -n kotoki get pod <pod> -o jsonpath='{.status.containerStatuses[0].lastState}'` (alasan `OOMKilled`?), `kubectl -n kotoki logs <pod> --previous`. Memori mendekati limit 2 GiB (`GOMEMLIMIT` 1800MiB): panel "Realtime memory per pod".
5. Slow consumer: korelasi dengan satu pod (`sum by (pod) (rate(kotoki_realtime_slow_consumer_total[5m]))`), dengan klien tertentu, atau dengan node/jaringan (gateway, [R11](R11-gateway-certificate.md)).

## Mitigasi

- Kapasitas: naikkan lantai replika lewat ScaledObject (jangan `kubectl scale`, HPA KEDA menimpanya), maksimum 20:

  ```bash
  kubectl -n kotoki patch scaledobject kotoki-realtime --type merge -p '{"spec":{"minReplicaCount":12}}'
  ```

  Scale-up KEDA tanpa stabilisasi (+4 pod atau +100% per 60 detik, ambil yang terbesar), scale-down lambat (1 pod per 300 detik setelah 600 detik). Kembalikan `minReplicaCount` ke 3 setelah reda.
- Pod tidak terjadwal: tambah node pool realtime; periksa kuota cloud dan zona (topologySpread zona `DoNotSchedule`).
- KEDA tak bisa membaca Prometheus: perbaiki `serverAddress` (`kube-prometheus-stack-prometheus.observability:9090`) atau sambungan; sementara itu fallback 15 replika aktif setelah 3 kegagalan berturut-turut.
- OOM: naikkan limit memori sementara (`kubectl -n kotoki set resources deployment/kotoki-realtime -c realtime --limits=memory=3Gi` lalu sinkronkan kembali lewat deploy; ubah `GOMEMLIMIT` proporsional) dan ambil profil heap: `PPROF_ENABLED=true` dibaca dari env semua pod Deployment (endpoint `/debug/pprof/` pada port ops 9090, tidak publik); aktifkan di staging bila bisa, di produksi hanya sementara dan matikan setelahnya; akses dengan `kubectl -n kotoki port-forward pod/<pod> 9090:9090` lalu `go tool pprof http://localhost:9090/debug/pprof/heap`. Tidak ada cara menyalakannya pada satu pod saja tanpa mengubah Deployment.
- Slow consumer: bila terpusat pada satu pod/node, kuras pod itu (`kubectl -n kotoki delete pod <pod>`; drain 40 detik dan klien reconnect); bila tersebar, periksa jaringan dan idle timeout gateway ([R11](R11-gateway-certificate.md)).
- Rollout yang memicu crash: `kubectl -n kotoki rollout undo deployment/kotoki-realtime`.

## Verifikasi pulih

- `rate(kotoki_realtime_connections_total{result="rejected_capacity"}[5m])` = 0 dan rasio kapasitas < 80% (atau KEDA menambah pod sampai di bawah 70 sesi per pod).
- Semua pod realtime `Ready` dan tidak restart lagi dalam 15 menit; `kotoki_realtime_slow_consumer_total` datar.
- Tidak ada sesi `FAILED` baru akibat insiden: panel "FAILED sessions per hour" di dashboard `Business integrity`.
- `minReplicaCount` kembali 3 dan setiap ubahan sementara (limit memori, `PPROF_ENABLED`) dikembalikan.
