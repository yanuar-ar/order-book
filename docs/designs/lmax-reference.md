# LMAX — Referensi Arsitektur & Pola

> Referensi konsep LMAX (London Multi-Asset Exchange) dan pola **Disruptor**, dipetakan eksplisit ke desain engine di `spot-orderbook-engine-design.md`. Untuk acuan di Claude Code.
> LMAX adalah asal-usul banyak keputusan desain kita (single-writer, event sourcing, ring buffer, mechanical sympathy). Dokumen ini menjelaskan *kenapa*-nya dan memetakannya ke komponen kita.

---

## 1. Apa itu LMAX & hasil kuncinya

LMAX = bursa ritel multi-aset (FX/CFD) yang mempublikasikan arsitekturnya (~2011, artikel Martin Fowler) dan meng-open-source library **Disruptor**. Hasil yang terkenal: **~6 juta order/detik pada satu thread** untuk logika bisnis, dengan latency mikrodetik — semua **in-memory, tanpa lock, tanpa database di hot path**.

Pelajaran inti yang kita warisi: *concurrency adalah musuh throughput; pindahkan kerja konkuren ke tepi, jadikan logika bisnis single-threaded & deterministik.*

---

## 2. Arsitektur inti: Business Logic Processor + input/output Disruptor

```
[Receiver/Gateway] → INPUT Disruptor ─┬─→ [Un-marshaller] ┐
                                       ├─→ [Journaller]    │ (paralel)
                                       └─→ [Replicator]    ┘
                                              │ sequence barrier: BLP menunggu
                                              ▼ journal + replikasi SELESAI
                                    ┌────────────────────────┐
                                    │ BUSINESS LOGIC PROCESSOR│  single-thread,
                                    │ (BLP) — in-memory,      │  deterministik
                                    │ event-driven            │
                                    └───────────┬────────────┘
                                                ▼
                              OUTPUT Disruptor → [Publisher/Marshaller] → klien
```

Tiga bagian: **input disruptor** (mengerjakan hal konkuren yang "kotor"), **BLP** (logika bisnis murni), **output disruptor** (publikasi hasil).

---

## 3. Business Logic Processor (BLP)

- **Single-threaded** — semua logika bisnis di satu thread. Tanpa lock → tanpa kontensi.
- **In-memory** — seluruh state di RAM; **tidak ada DB di hot path**.
- **Event-driven** — mengonsumsi event dari input disruptor satu per satu.
- **Deterministik** — input sama → output & state sama. Inilah yang memungkinkan **replay** & **replikasi**.
- Mencapai ~6 juta TPS karena: tanpa lock, tanpa DB, tanpa I/O jaringan di thread, ramah cache, satu penulis.
- **Recovery via event sourcing**: stream event input di-journal; saat restart, replay untuk membangun ulang state; snapshot periodik membatasi waktu replay.

---

## 4. Input Disruptor & consumer-nya

Sisi input mengerjakan kerja konkuren agar BLP tidak perlu. Beberapa consumer berjalan **paralel** atas ring buffer yang sama:
- **Un-marshaller / Receiver** — byte jaringan → objek bisnis (deserialisasi).
- **Journaller** — tulis event ke disk (durabilitas).
- **Replicator** — kirim event ke node backup (HA).

**Sequence barrier:** BLP **menunggu** sampai journalling **dan** replikasi atas suatu event selesai sebelum memprosesnya. Jadi BLP tak pernah memproses event yang belum durable + ter-replikasi → jaminan *"persist sebelum proses/lepas output"*. Journalling & replikasi berjalan paralel (dependency dua-cabang), lalu BLP menunggu keduanya — paralelisme inilah sumber throughput.

---

## 5. Output Disruptor

Hasil BLP masuk output disruptor, dikonsumsi **publisher** (marshalling hasil → jaringan, kirim ke klien). Memisahkan publikasi (I/O) dari logika bisnis.

---

## 6. Pola Disruptor (inti teknis)

Library messaging antar-thread; pengganti queue.

| Konsep | Penjelasan |
|---|---|
| **Ring buffer** | Array pra-alokasi, ukuran power-of-2 (index = `seq & (size-1)`). Entry **dipakai ulang** → tanpa garbage/alokasi di steady state (krusial untuk hindari GC di JVM). |
| **Sequence** | Nomor monoton menandai tiap slot/event. Tiap consumer melacak sequence-nya sendiri (sejauh mana ia memproses). |
| **Sequence barrier** | Consumer menunggu sampai event tersedia **dan** consumer dependen sudah memproses (untuk ordering/dependency). |
| **Gating sequence** | Producer tak boleh menimpa slot yang belum dikonsumsi consumer terlambat → lacak consumer terlambat. |
| **Single Writer Principle** | Hindari kontensi: satu penulis per data. BLP = penulis tunggal state bisnis. |
| **Mechanical sympathy** | Desain selaras hardware: **cache-line padding** (sequence di-pad ke 64 byte → hindari **false sharing**), akses memori sekuensial (prefetch), tanpa lock. |
| **Tanpa lock** | Memory barrier untuk visibilitas/ordering; **CAS** hanya untuk multi-producer (single-producer cukup memory barrier). |
| **Batching effect** | Consumer yang tertinggal mengambil **semua** event tersedia sekaligus & memproses sebagai batch → mengejar otomatis. Throughput naik di bawah beban tanpa mengorbankan latency saat sepi (self-smoothing). |
| **Wait strategy** | Cara consumer menunggu saat kosong: `BusySpin` (latency terendah, bakar CPU), `Yielding`, `Sleeping`, `Blocking` (hemat CPU). Trade-off latency vs CPU. |
| **Multicast / dependency graph** | Banyak consumer membaca event **yang sama** (beda dari queue: tiap item ke satu consumer). Consumer disusun sebagai graf dependency via barrier (mis. journaller & replicator paralel → BLP menunggu keduanya → publisher setelah BLP). |

---

## 7. Kenapa BUKAN queue

LMAX menemukan queue tradisional (mis. `ArrayBlockingQueue`) bermasalah:
- **Titik kontensi**: head & tail ditulis thread berbeda → cache-line bouncing + lock.
- Cenderung **selalu penuh atau kosong** (tak ada manfaat buffering).
- Alokasi/GC (queue berbasis linked-list).
- Mencampur kepentingan producer & consumer.

Disruptor menggantinya dengan **ring buffer + sequence + barrier**, menghapus kontensi. (Inilah alasan di dokumen engine kita memilih SPSC ring, bukan channel Go.)

---

## 8. Event sourcing, snapshot, replay, replikasi

- **Stream event input ter-journal = sumber kebenaran.**
- **Crash** → replay event ter-journal untuk membangun ulang state BLP.
- **Snapshot** periodik membatasi waktu replay.
- **Replikasi**: master BLP + replica BLP mengonsumsi stream input sama → hot standby; master gagal → replica ambil alih.
- **Determinisme** adalah yang membuat replay & replikasi menghasilkan state identik. (Persis kontrak determinisme engine kita.)

---

## 9. Pemetaan LMAX → desain engine kamu

Ini bagian paling berguna untuk Claude Code — tiap konsep LMAX punya padanan di dokumen kita:

| LMAX | Padanan di engine/service kita | Dokumen |
|---|---|---|
| **Business Logic Processor** | core deterministik: sequencer + balance authority + market shard | engine §1–§3 |
| **Input disruptor** | ring ingress + SPSC antar-tahap | engine §5 |
| **Consumer: Journaller** | **WAL** (mmap append + CRC) | engine §6 |
| **Consumer: Replicator** | **replikasi Raft** (quorum-commit) | engine §16 |
| **Consumer: Un-marshaller** | **ingress adapter** (decode gRPC → Command) | service §5.1 |
| **Output disruptor** | event stream → publisher → event bus | service §3 |
| **Sequence barrier (journal+replicate sebelum BLP)** | aturan *"persist sebelum lepas output"* / **match-on-commit** | engine §16.4 |
| **Event sourcing + snapshot + replay** | WAL + snapshot + replay recovery | engine §6 |
| **Single Writer Principle** | balance authority single-writer + order book single-owner | engine §3, §7 |
| **Mechanical sympathy (padding, no-lock)** | SPSC cache-line padded, zero-alloc, pin core | engine §5, §12 |
| **Ring buffer** | SPSC ring (gaya rigtorp) | engine §5 |
| **Batching effect** | drain batch di sequencer/consumer; group-commit WAL | engine §6.1 |

---

## 10. Prinsip LMAX yang diadopsi

1. **Single Writer Principle** — satu penulis per state → tanpa lock, tanpa kontensi.
2. **Mechanical sympathy** — desain selaras CPU/cache (padding, sekuensial, tanpa lock).
3. **Logika bisnis single-threaded & in-memory**; dorong concurrency ke **tepi** (consumer disruptor / ingress adapter).
4. **Event sourcing** untuk durabilitas + replikasi via **replay deterministik**.
5. **Ring buffer + sequence**, bukan queue, untuk handoff antar-thread.
6. **Batching effect** untuk *load smoothing* otomatis.
7. **Persist sebelum lepas output** (sequence barrier journal+replicate sebelum proses).

---

## 11. Catatan untuk implementasi Go (vs JVM LMAX)

- LMAX di **JVM**: Disruptor menukar entry pra-alokasi demi **menghindari GC**. Di **Go**, padanannya = **zero-alloc + arena off-heap + GC off saat sesi** (engine §12). Tujuan sama, mekanisme beda.
- **SPSC vs full Disruptor di Go**:
  - Untuk handoff **satu-ke-satu** (parse→match, match→publish) → cukup **SPSC ring** (engine §5). Lebih sederhana.
  - Untuk **fan-out satu-ke-banyak dengan dependency** (satu stream input dikonsumsi journaller + replicator + matcher, lalu barrier) → **pola Disruptor penuh** (multicast ring + sequence barrier) lebih tepat daripada menyalin ke banyak SPSC. Pertimbangkan port Disruptor Go atau implementasi multicast-ring sendiri di titik ini.
- **Perbedaan desain:** BLP LMAX = **satu thread untuk SELURUH logika bisnis** (satu domain in-memory). Desain kita **men-shard market antar-core** — ini *refinement* (isolasi + paralelisme per market). Baseline LMAX (single-thread total) tetap valid & lebih sederhana bila volume muat; sharding adalah ekstensi saat dibutuhkan (engine §"sharding").
- **Wait strategy**: padanan `BusySpinWaitStrategy` LMAX = busy-spin di core terisolasi kita (engine §12). Trade-off latency vs CPU yang sama berlaku.

---

## Lampiran — bacaan & istilah
- Konsep kanonik: *Disruptor, ring buffer, sequence barrier, gating sequence, single writer principle, mechanical sympathy, false sharing, batching effect, wait strategy, multicast/dependency graph, Business Logic Processor, event sourcing*.
- Pemetaan ke implementasi → §9 dokumen ini; detail mekanik → `spot-orderbook-engine-design.md`.
