# ARCHITECTURE.md — Spot Order-Book Matching Engine

> Mesin *matching* order-book **spot** yang deterministik, *event-sourced*, dan
> *zero-alloc* di jalur panas, ditulis dalam Go murni (zero-dependency).
> Dokumen ini menjelaskan **bagaimana** engine bekerja: aliran data, ring
> buffer (siapa produsen, siapa consumer), cara kerja sequencer, balance
> authority, matching, WAL, snapshot/recovery, dan strategi multi-core —
> dipetakan eksplisit ke pola **LMAX Disruptor**.
>
> Dokumen ini menggambarkan **implementasi nyata** di `internal/`. Untuk
> spesifikasi rancangan & rasionalisasi keputusan, lihat
> [`docs/designs/spot-orderbook-engine-design.md`](docs/designs/spot-orderbook-engine-design.md)
> dan pemetaan konsep LMAX di
> [`docs/designs/lmax-reference.md`](docs/designs/lmax-reference.md).

---

## Daftar Isi

1. [Filosofi: Mengapa LMAX](#1-filosofi-mengapa-lmax)
2. [Peta Komponen Tingkat Tinggi](#2-peta-komponen-tingkat-tinggi)
3. [Pemetaan LMAX → Engine ini](#3-pemetaan-lmax--engine-ini)
4. [Kontrak Determinisme](#4-kontrak-determinisme)
5. [SPSC Ring Buffer: Produsen & Consumer](#5-spsc-ring-buffer-produsen--consumer)
6. [Sequencer: Otoritas Urutan Tunggal](#6-sequencer-otoritas-urutan-tunggal)
7. [Balance Authority: Single-Writer Ledger](#7-balance-authority-single-writer-ledger)
8. [Order Book & Matching Engine](#8-order-book--matching-engine)
9. [Write-Ahead Log (Journaller)](#9-write-ahead-log-journaller)
10. [Snapshot & Recovery](#10-snapshot--recovery)
11. [Siklus Hidup Sebuah Order (End-to-End)](#11-siklus-hidup-sebuah-order-end-to-end)
12. [Strategi Multi-Core: Serial vs Paralel](#12-strategi-multi-core-serial-vs-paralel)
13. [Performa & Zero-Alloc](#13-performa--zero-alloc)
14. [Peta Paket](#14-peta-paket)

---

## 1. Filosofi: Mengapa LMAX

LMAX (London Multi-Asset Exchange) membuktikan ~2011 bahwa satu thread bisa
memproses **~6 juta order/detik** dengan latency mikrodetik — **tanpa lock,
tanpa database di hot path, semuanya in-memory**. Pelajaran intinya:

> **Concurrency adalah musuh throughput.** Dorong kerja konkuren ("kotor") ke
> *tepi*; jadikan logika bisnis **single-threaded, in-memory, dan
> deterministik**.

Engine ini mewarisi empat prinsip itu secara langsung:

| Prinsip LMAX | Implementasi di sini |
|---|---|
| **Single Writer Principle** | Sequencer satu-satunya pemberi `Seq`; ledger satu-satunya otoritas dana; tiap book hanya dimiliki satu shard. |
| **Mechanical sympathy** | SPSC ring *cache-line padded* (hindari false sharing), tipe POD bebas-pointer, arena pra-alokasi, GC dimatikan saat sesi. |
| **Event sourcing** | WAL = sumber kebenaran. State = fungsi deterministik dari log command terurut → replay membangun ulang state byte-identik. |
| **Ring buffer, bukan queue** | Handoff antar-tahap memakai SPSC ring lock-free, bukan channel Go. |

**Perbedaan penting dari LMAX klasik:** LMAX menjalankan SELURUH logika bisnis di
satu thread. Engine ini punya **dua topologi**:

- **Serial (v1, default)** — sequencer + ledger + matching berjalan **inline di
  satu goroutine**. Ini paling setia pada model BLP LMAX dan paling sederhana
  untuk dibuktikan benar.
- **Paralel** — market di-*shard* ke beberapa goroutine **worker yang
  di-pin ke core berbeda**, sementara jalur kontrol (sequencer + ledger) tetap
  satu penulis. Ini *refinement* untuk isolasi & paralelisme per-market.

---

## 2. Peta Komponen Tingkat Tinggi

```mermaid
flowchart TB
    subgraph EDGE["TEPI (kerja konkuren)"]
        ING["Ingress / Bench<br/>(banyak produsen)"]
    end

    subgraph BLP["BUSINESS LOGIC PROCESSOR (deterministik)"]
        direction TB
        SEQ["SEQUENCER<br/>• assign Seq + TsNanos<br/>• journal SEMUA command<br/>• urutkan settlement<br/>• durable-ack barrier"]
        LED["BALANCE AUTHORITY<br/>(single-writer ledger)<br/>available / reserved"]
        subgraph SHARDS["MARKET SHARDS"]
            S0["Shard BTC/USDT<br/>book + matching"]
            S1["Shard ETH/USDT<br/>book + matching"]
            S2["Shard SOL/USDT<br/>book + matching"]
        end
        SEQ -->|"OnCommand"| LED
        LED -->|"funded order"| SHARDS
        SHARDS -.->|"Fill (settlement)"| SEQ
        SHARDS -.->|"aktivasi Stop (reinject)"| SEQ
    end

    subgraph DUR["DURABILITAS"]
        WAL[("WAL<br/>segmented + CRC<br/>group-commit fsync")]
        SNAP[("Snapshot<br/>periodik")]
    end

    subgraph OUT["OUTPUT"]
        ACK["Acks (di-gate oleh<br/>DurableSeq)"]
    end

    ING -->|"SPSC RingCommand"| SEQ
    SEQ -->|"Append + Sync"| WAL
    BLP -.->|"state"| SNAP
    SEQ -->|"rilis saat durable"| ACK
```

**Lima peran:**

1. **Ingress / Bench** — sumber `Command`. Banyak produsen, tiap produsen punya
   SPSC sendiri (lihat §5 dan §6).
2. **Sequencer (+ WAL)** — penulis tunggal urutan global. Memberi `Seq`
   monotonik + `TsNanos`, men-*journal* **setiap** command (eksternal **dan**
   aktivasi stop), lalu meneruskan ke ledger. Juga mengurutkan `Fill` sebelum
   settlement, dan menahan ack sampai durable.
3. **Balance Authority** — satu ledger `available/reserved` bersama untuk semua
   market. *Single writer* → konsisten lintas market tanpa lock.
4. **Market Shard** — order book + matching + tabel stop, per market. Tidak
   menyentuh dana; hanya menghasilkan `Fill` dan aktivasi stop.
5. **Output** — `Ack` dirilis hanya untuk command yang `Seq`-nya **sudah durable
   di WAL** (lihat *durable-ack barrier*, §6).

---

## 3. Pemetaan LMAX → Engine ini

```mermaid
flowchart LR
    subgraph LMAX["Pola LMAX Disruptor"]
        direction TB
        L_IN["Input Disruptor"]
        L_UM["Un-marshaller"]
        L_JR["Journaller"]
        L_RP["Replicator"]
        L_BLP["Business Logic<br/>Processor (BLP)"]
        L_OUT["Output Disruptor<br/>→ Publisher"]
        L_IN --> L_UM & L_JR & L_RP --> L_BLP --> L_OUT
    end

    subgraph ENG["Engine ini"]
        direction TB
        E_IN["SPSC RingCommand<br/>(ingress)"]
        E_UM["DecodeCommand<br/>(codec WAL)"]
        E_JR["WAL Writer<br/>(internal/wal)"]
        E_RP["Raft replicator<br/>(out-of-scope v1)"]
        E_BLP["Sequencer + Ledger<br/>+ Market Shards"]
        E_OUT["Acks (gated<br/>oleh DurableSeq)"]
        E_IN --> E_UM & E_JR --> E_BLP --> E_OUT
        E_RP -.->|"v1: belum ada"| E_BLP
    end

    L_UM -.-> E_UM
    L_JR -.-> E_JR
    L_RP -.-> E_RP
    L_BLP -.-> E_BLP
    L_OUT -.-> E_OUT
```

| Konsep LMAX | Padanan di engine | Lokasi |
|---|---|---|
| **Business Logic Processor** | sequencer + balance + market shard | `internal/sequencer`, `internal/balance`, `internal/market` |
| **Input disruptor** | SPSC ring ingress + reinject | `internal/spsc` |
| **Journaller** | WAL (segmented, CRC, group-commit) | `internal/wal` |
| **Replicator** | replikasi Raft (**out-of-scope v1**) | — |
| **Un-marshaller** | `types.DecodeCommand` (codec WAL) | `internal/types/codec.go` |
| **Sequence barrier (persist sebelum lepas output)** | *durable-ack barrier* — ack hanya rilis di bawah `DurableSeq` | `internal/sequencer/sequencer.go` |
| **Single Writer Principle** | ledger single-writer + book single-owner | `internal/balance`, `internal/orderbook` |
| **Mechanical sympathy** | SPSC cache-line padded, zero-alloc, pin core | `internal/spsc`, `internal/platform` |
| **Batching effect** | drain batch fill + group-commit WAL | `internal/sequencer`, `internal/wal` |

> **Catatan jujur:** *Replicator* (HA via Raft) belum ada di v1. *Output
> disruptor* di v1 berupa buffer ack yang dirilis bertahap sesuai watermark
> durable, bukan ring publisher penuh. Keduanya dirancang masuk di fase HA.

---

## 4. Kontrak Determinisme

Seluruh sistem adalah **state machine deterministik** di atas satu log event
terurut. Inilah yang membuat replay & (kelak) replikasi menghasilkan state
**byte-identik**. Enam aturan yang dijaga kode:

1. **Satu sumber urutan.** Hanya sequencer yang memberi `Seq`
   (`sequenceAndRoute`, `sequencer.go:274`). Tidak ada komponen lain mengarang
   urutan.
2. **Timestamp di-capture sekali.** `c.TsNanos = s.clock()` dibaca **hanya** di
   sequencer, tepat satu kali per command (`sequencer.go:277`). Replay memakai
   nilai tersimpan — tidak pernah membaca jam lagi.
3. **Tiap komponen = fungsi murni** dari aliran input terurutnya. Tanpa
   randomness, tanpa I/O, tanpa wall-clock di dalam logika.
4. **Antar-komponen lewat SPSC FIFO** → urutan terjaga di tiap link.
5. **Fill diurutkan deterministik** oleh kunci `(AggressorSeq, MatchIndex)` —
   properti *data*, bukan waktu kedatangan (`drainFills`,
   `sequencer.go:235`). Walau matching berjalan di goroutine terpisah (mode
   paralel), settlement-nya selalu dalam urutan kunci yang sama.
6. **Stop activation di-journal juga.** Aktivasi stop di-*reinject* sebagai
   command ber-`Seq` baru dan ditulis ke WAL. Saat replay, aktivasi
   di-*suppress* (sudah ada di log) agar tidak ter-trigger dua kali
   (`SuppressStops`).

> **Konsekuensi bisnis yang benar:** kamu tak bisa menjual aset yang fill-nya
> belum disetel pada posisi `Seq` lebih awal. Ini deterministik (tergantung
> urutan `Seq`, bukan timing) — dan memang perilaku spot yang benar.

---

## 5. SPSC Ring Buffer: Produsen & Consumer

Engine **tidak memakai channel Go** di jalur panas. Handoff antar-tahap memakai
**SPSC ring** (Single-Producer Single-Consumer) lock-free bergaya *rigtorp* —
padanan langsung ring buffer Disruptor.

### 5.1 Struktur (mechanical sympathy)

`internal/spsc/ring.go`:

```go
type Ring[T any] struct {
    _    [64]byte
    head atomic.Uint64 // dibaca consumer, maju saat Pop
    _    [56]byte       // padding → head & tail beda cache line
    tail atomic.Uint64  // dibaca producer, maju saat Push
    _    [56]byte
    buf  []T
    mask uint64
}
```

**Kenapa padding?** `head` ditulis consumer, `tail` ditulis producer. Tanpa
padding keduanya bisa jatuh di *cache line* yang sama → **false sharing**:
tulisan satu pihak meng-invalidasi cache pihak lain. Padding 64-byte memastikan
keduanya di line berbeda → tiap pihak menulis ke line-nya sendiri tanpa
mengganggu.

### 5.2 Push (hanya producer) & Pop (hanya consumer)

```mermaid
flowchart LR
    subgraph PROD["PRODUCER (1 goroutine)"]
        P1["Load tail"] --> P2{"tail - head<br/>>= cap?"}
        P2 -->|"ya"| PF["return false<br/>(penuh)"]
        P2 -->|"tidak"| P3["buf[tail & mask] = v"]
        P3 --> P4["Store tail+1<br/>(PUBLISH)"]
    end
    subgraph CONS["CONSUMER (1 goroutine)"]
        C1["Load head"] --> C2{"head == tail?"}
        C2 -->|"ya"| CF["return false<br/>(kosong)"]
        C2 -->|"tidak"| C3["out = buf[head & mask]"]
        C3 --> C4["Store head+1<br/>(PUBLISH)"]
    end
    P4 -.->|"tail terlihat<br/>oleh consumer"| C2
    C4 -.->|"head terlihat<br/>oleh producer"| P2
```

- **Kapasitas wajib power-of-two** → indeks `seq & mask` tanpa modulo.
- **Producer tak pernah menulis `head`; consumer tak pernah menulis `tail`** —
  inilah kunci lock-free SPSC. Tiap pihak hanya membaca counter pihak lain
  sebagai *hint* penuh/kosong.
- Atomik Go bersifat *sequentially consistent* → tidak perlu barrier eksplisit.
- Entry **dipakai ulang** (`buf` pra-alokasi) → nol garbage di steady state.

### 5.3 Ring konkret & siapa produsen/consumer

`internal/spsc/concrete.go` mendefinisikan alias tipe-konkret untuk link
terpanas (menghindari overhead generic di call site):

```go
type (
    RingCommand = Ring[types.Command]
    RingFunded  = Ring[types.FundedOrder]
    RingFill    = Ring[types.Fill]
    RingAck     = Ring[types.Ack]
)
```

| Ring | **Produsen** | **Consumer** | Isi |
|---|---|---|---|
| `ingress` (`RingCommand`) | klien eksternal / bench | Sequencer | order, cancel, amend, deposit, withdraw |
| `reinject` (`RingCommand`) | Market shard (lewat `Sink` aktivasi stop) | Sequencer | aktivasi stop → command baru (prioritas) |
| `reqs[w]` (`Ring[wreq]`) | jalur kontrol (`remoteShard`) | Worker | request matching (Submit/Cancel/Amend/LastPrice) |
| `resps[w]` (`Ring[wresp]`) | Worker | jalur kontrol (`remoteShard`) | hasil matching (fills, ok, aktivasi) |
| `fills[m]` (`RingFill`) | Market shard | Sequencer | fill untuk settlement urut `(AggressorSeq, MatchIndex)` |

> `reqs`/`resps` hanya dipakai di **topologi paralel** (§12). Di topologi serial,
> matching dipanggil inline tanpa ring perantara.

---

## 6. Sequencer: Otoritas Urutan Tunggal

`internal/sequencer/sequencer.go`. Inti dari BLP: satu goroutine, penulis
tunggal urutan global. Loop-nya (`Step()`) menjalankan fase **deterministik**
berikut setiap iterasi:

```mermaid
flowchart TB
    START(["Step()"]) --> FATAL{"fatal<br/>ter-latch?"}
    FATAL -->|"ya"| NOOP["return false<br/>(no-op selamanya)"]
    FATAL -->|"tidak"| DRAIN["1. drainFills()<br/>tiriskan SEMUA fill ring →<br/>sort by (AggressorSeq, MatchIndex) →<br/>OnSettlement tiap fill"]
    DRAIN --> REIN["2. Tiriskan reinject ring<br/>(aktivasi stop, PRIORITAS)<br/>tiap command: sequenceAndRoute"]
    REIN --> EXT{"3. pollExternal()<br/>round-robin antar<br/>input ring"}
    EXT -->|"dapat command"| ROUTE["sequenceAndRoute(c)"]
    EXT -->|"kosong & ada<br/>unsynced"| FLUSHD["flush() segera<br/>(latency rendah saat sepi)"]
    ROUTE --> CAP{"unsynced >=<br/>flushCap?"}
    CAP -->|"ya"| FLUSHC["flush()<br/>(group-commit, batching)"]
    CAP -->|"tidak"| END(["return"])
    FLUSHD --> END
    FLUSHC --> END
```

### 6.1 Assign Seq + timestamp + journal (atomik secara logis)

`sequenceAndRoute` (`sequencer.go:274`) adalah jantungnya:

```go
func (s *Sequencer) sequenceAndRoute(c *types.Command) error {
    s.seq++
    c.Seq = s.seq
    c.TsNanos = s.clock()              // satu-satunya pembacaan jam
    n := types.EncodeCommandInto(s.payloadBuf[:], *c)
    if err := s.journal.Append(wal.Record{
        Seq: uint64(s.seq), TsNanos: c.TsNanos,
        Type: uint16(c.Type), Payload: s.payloadBuf[:n],
    }); err != nil {
        return err                     // GAGAL journal → JANGAN route, latch fatal
    }
    s.router.OnCommand(*c)             // baru teruskan ke ledger + matching
    return nil
}
```

**Urutan ini krusial:** journal **dulu**, route **kemudian**. Jika `Append`
gagal, command tidak pernah diterapkan dan tidak ada ack — WAL tetap sumber
kebenaran.

### 6.2 MPSC fan-in: round-robin, bukan queue ber-CAS

Banyak produsen → satu sequencer. Engine **tidak** memakai satu queue MPSC
ber-CAS (titik kontensi). Polanya: **tiap produsen punya SPSC sendiri, sequencer
poll bergiliran**:

```go
func (s *Sequencer) pollExternal() (types.Command, bool) {
    n := len(s.inputs)
    for i := 0; i < n; i++ {
        idx := (s.rr + i) % n
        var c types.Command
        if s.inputs[idx].Pop(&c) {
            s.rr = (idx + 1) % n   // cursor maju hanya saat sukses
            return c, true
        }
    }
    return types.Command{}, false
}
```

Tiap link tetap SPSC (tercepat); sequencer yang melakukan penggabungan + assign
`Seq`. Cursor `rr` menjamin keadilan antar-produsen.

### 6.3 Prioritas reinject

Aktivasi stop di-*reinject* lebih dulu daripada command eksternal dalam satu
`Step()` (fase 2 sebelum fase 3). Ini memberi interleaving deterministik:
trigger yang lahir dari fill harus diproses sebelum command eksternal
berikutnya.

### 6.4 Pengurutan fill untuk settlement

`drainFills` (`sequencer.go:235`) menarik fill dari **semua** market ring,
menggabungnya, lalu **mengurutkan**:

```go
sort.Slice(batch, func(i, j int) bool {
    if batch[i].AggressorSeq != batch[j].AggressorSeq {
        return batch[i].AggressorSeq < batch[j].AggressorSeq
    }
    return batch[i].MatchIndex < batch[j].MatchIndex
})
for _, fl := range batch {
    s.router.OnSettlement(fl)
}
```

Karena urutan settlement ditentukan **data** (`AggressorSeq`, `MatchIndex`),
bukan timing kedatangan, hasilnya identik antar-run dan saat replay.

### 6.5 Durable-ack barrier (sequence barrier ala LMAX)

Padanan langsung *"persist sebelum lepas output"* LMAX. Bersifat **output-side
saja** — tidak pernah di-journal, tidak memengaruhi `Seq`/timestamp/urutan fill,
jadi replay tetap byte-identik berapa pun cadence-nya.

```mermaid
flowchart LR
    A["command di-route<br/>(ack masuk buffer)"] --> B["unsynced++"]
    B --> C{"trigger flush?"}
    C -->|"unsynced >= flushCap<br/>(batas batch)"| D["flush()"]
    C -->|"ring drained<br/>(idle, latency rendah)"| D
    D --> E["journal.Sync()<br/>(fsync)"]
    E --> F["durableSeq = last Seq<br/>unsynced = 0"]
    F --> G["Ack dengan Seq <= durableSeq<br/>BOLEH dirilis"]
```

- `flushCap` (default **64**) = langit-langit *group-commit*. Makin besar →
  throughput durable naik, latency durable-ack naik. Inilah **batching effect**
  LMAX: di bawah beban, banyak command meng-amortisasi satu `fsync`.
- `DurableSeq()` jadi gerbang rilis ack. Ack di atas watermark = *speculative*,
  ditahan.
- **Fail-stop:** kegagalan `Append`/`Sync` di-*latch* ke `s.fatal`; sesudahnya
  `Step()` jadi no-op dan tak ada ack dirilis. WAL rusak = engine berhenti maju,
  bukan melanjutkan diam-diam.

---

## 7. Balance Authority: Single-Writer Ledger

`internal/balance`. Satu ledger `available/reserved` bersama untuk **semua**
market. Karena single-writer, konsisten lintas market tanpa lock — USDT yang
sama tak bisa dipakai dua kali di BTC/USDT dan ETH/USDT.

### 7.1 Satu aliran event bertag

Reservasi & settlement mengalir lewat **satu** stream bertag → interleaving
tetap & deterministik (`internal/balance/event.go`):

```go
const (
    EvReserve  EventKind = iota // kunci dana untuk order baru
    EvSettle                    // terapkan satu fill (pembeli bayar, penjual terima)
    EvRelease                   // lepas sisa reservasi saat selesai/cancel
    EvDeposit                   // kredit available
    EvWithdraw                  // debit available
)
```

```mermaid
flowchart TB
    subgraph LEDGER["Ledger (single writer)"]
        BAL["bal: map[account|asset] → {Available, Reserved}"]
        RES["res: map[OrderID] → reservation"]
        FEE["fees: map[asset] → akumulasi fee"]
    end
    EVR["EvReserve"] -->|"Available -= cost<br/>Reserved += cost"| LEDGER
    EVS["EvSettle"] -->|"buyer: Reserved -= terpakai, base += qty<br/>seller: Reserved -= qty, quote += proceeds"| LEDGER
    EVL["EvRelease"] -->|"Reserved -= sisa<br/>Available += sisa"| LEDGER
    EVD["EvDeposit"] -->|"Available += amount"| LEDGER
    EVW["EvWithdraw"] -->|"cek ≥ → Available -= amount"| LEDGER
```

### 7.2 Aturan pembulatan uang (satu bug = uang hilang)

Semua aritmetika `price × qty` di `int64` ter-skala via `MulDiv` dengan
*intermediate* 128-bit (`internal/types/money.go`) sehingga produk tak pernah
overflow sebelum dibagi. Arah pembulatan **disengaja**:

| Operasi | Arah | Alasan |
|---|---|---|
| **Reservasi** (`EvReserve`) | **round UP** | reservasi pembeli **tak pernah under-cover** fill yang akan datang |
| **Settlement** (`EvSettle`) | **round DOWN** | menguntungkan engine; tak ada uang tercipta |

```go
// Reserve: Notional(..., roundUp=true), Fee(..., roundUp=true)
// Settle:  Notional(..., roundUp=false), Fee(..., roundUp=false)
```

---

## 8. Order Book & Matching Engine

### 8.1 Order book: arena + intrusive FIFO (cache-friendly, zero-GC-scan)

`internal/orderbook/book.go`. Order disimpan di **arena** `[]orderNode` dan
dirujuk lewat indeks `uint32` (**bukan pointer**) → ramah cache, nol scanning
GC. Tiap price level adalah **doubly-linked list intrusive** untuk FIFO.

```mermaid
flowchart TB
    subgraph BOOK["Book (per market)"]
        ARENA["arena []orderNode<br/>(pra-alokasi)"]
        FREE["free []uint32<br/>(free-list daur ulang slot)"]
        IDX["idIndex: OrderID → arena idx"]
        subgraph ASKS["askPrices (naik) — best = pertama"]
            A1["100.05 → [o7 → o9]"]
            A2["100.06 → [o3]"]
        end
        subgraph BIDS["bidPrices (naik) — best = terakhir"]
            B1["100.03 → [o1 → o4 → o8]"]
            B2["100.04 → [o5]"]
        end
        LAST["lastPrice (untuk trigger stop)"]
    end
```

`orderNode` adalah POD bebas-pointer; iceberg dipecah `display` (tampak) +
`hidden` (cadangan) + `peak` (ukuran chunk replenish). FIFO dijaga via
`next/prev` indeks arena; `NilIdx = 0xFFFFFFFF` menandai ujung.

### 8.2 Inti matching: price-time priority

`internal/matching/match.go`. Entry point `Submit`:

```go
func (e *Engine) Submit(o types.FundedOrder) Result {
    if o.OrdType == types.Stop || o.OrdType == types.StopLimit {
        e.addStop(o)                 // simpan off-book, Pending
        return Result{Pending: true}
    }
    res := e.matchActive(o)          // sapu level lawan, FIFO dalam level
    e.triggerStops()                 // cek trigger setelah lastPrice berubah
    return res
}
```

Order agresor menyapu level lawan dari harga terbaik, FIFO dalam level, sampai
habis atau harga tak lagi cocok. **Fill selalu pada harga maker (resting)**.

### 8.3 Delapan "order type" = 4 OrderType × TIF × Flags

Enum `OrderType` hanya empat (`Limit`, `Market`, `Stop`, `StopLimit`), tapi
kombinasi dengan `TIF` (GTC/IOC/FOK) dan `Flags` (PostOnly/Iceberg) menghasilkan
delapan perilaku yang disebut di spec:

```mermaid
flowchart TB
    SUB["Submit(FundedOrder)"] --> T{"OrdType?"}
    T -->|"Stop / StopLimit"| STOP["Simpan di tabel stops<br/>→ Pending (off-book)"]
    T -->|"aktif"| PO{"FlagPostOnly &<br/>akan cross?"}
    PO -->|"ya"| REJ1["REJECT<br/>ReasonPostOnlyCross"]
    PO -->|"tidak"| FOK{"TIF == FOK?"}
    FOK -->|"ya & tak penuh<br/>(MatchableQty)"| REJ2["REJECT total<br/>ReasonFOKUnfillable<br/>(book tak berubah)"]
    FOK -->|"cukup / bukan FOK"| LOOP["Loop matching<br/>(price-time, FIFO)<br/>+ Self-Trade Prevention"]
    LOOP --> REM{"ada sisa?"}
    REM -->|"Market / IOC"| CANCEL["sisa dibatalkan<br/>(tak pernah rest)"]
    REM -->|"Limit/GTC"| REST["rest di book<br/>(iceberg: simpan hidden)"]
    REM -->|"habis"| DONE["selesai"]
```

| Perilaku | Mekanisme |
|---|---|
| **Limit (GTC)** | match sebisanya; sisa **rest** di `price`. |
| **Market** | sapu tanpa batas harga; sisa **dibatalkan**. Market-buy dibatasi `MaxQuote` (budget). |
| **IOC** | match segera; sisa **cancel** (tak rest). |
| **FOK** | pra-cek `MatchableQty` — bila tak bisa penuh → **reject total, tanpa mutasi**. |
| **Post-Only** | bila akan cross → **reject**; bila tidak → rest penuh (jamin maker). |
| **Iceberg** | hanya `display` tampak; saat habis & `hidden>0` → replenish dari hidden lalu **re-queue di tail** (kehilangan prioritas waktu — standar). |
| **Stop** | off-book; trigger saat `lastPrice` lewat `StopPrice` → jadi **Market**. |
| **Stop-Limit** | off-book; trigger → jadi **Limit** pada `Price`. |

Plus **Self-Trade Prevention (STP):** bila order lawan berikutnya milik akun yang
sama, sisa agresor dibatalkan (`STP: true`).

### 8.4 Trigger stop: re-injeksi sebagai command baru (anti-rekursi)

```mermaid
sequenceDiagram
    participant M as Matcher
    participant ST as Tabel stops
    participant SK as Sink
    participant SEQ as Sequencer

    M->>M: fill terjadi → update lastPrice
    M->>ST: triggerStops()
    Note over ST: buy-stop: last ≥ stopPrice<br/>sell-stop: last ≤ stopPrice
    ST->>ST: sort yang ter-trigger by Seq (deterministik)
    ST->>SK: activationCommand() (Stop→Market, StopLimit→Limit)
    SK->>SEQ: Push ke reinject ring
    Note over SEQ: dapat Seq BARU → masuk pipeline normal<br/>(di-journal, di-route ulang)
```

Aktivasi **tidak** diproses inline (hindari rekursi tak terbatas). Ia jadi
command baru ber-`Seq`, di-journal, dan diproses di `Step()` berikutnya dengan
prioritas reinject — sehingga deterministik dan tahan-replay.

---

## 9. Write-Ahead Log (Journaller)

`internal/wal`. Journal append-only, ter-*segment*, ber-CRC, dengan replay
tahan-*torn-tail* dan tahan-*gap*.

> v1 memakai I/O `os.File` ber-buffer + group-commit berbasis `Sync`. Jalur cepat
> `mmap` dari rancangan ditangguhkan ke fase performa; framing, segment, dan
> semantik replay (bagian yang menentukan korektnes) independen dari pilihan itu.

### 9.1 Format record (header 28 byte + payload)

```
 [0:8]   Seq        uint64
 [8:16]  TsNanos    int64
 [16:18] Type       uint16
 [18:20] Flags      uint16
 [20:24] PayloadLen uint32
 [24:28] CRC32      uint32  (atas payload)
 [28:]   Payload    (Command ter-encode)
```

### 9.2 Group-commit: satu `write` + satu `fsync` per batch

```go
func (w *Writer) Append(r Record) error {
    // frame ke encBuf, buffer di memori (BELUM durable)
    w.pending = append(w.pending, enc...)
    return nil
}
func (w *Writer) Sync() error {   // titik group-commit
    w.flushPending()              // satu write(2) untuk seluruh batch
    return w.cur.Sync()           // satu fsync
}
```

Mem-batch **syscall write** (bukan hanya fsync) inilah yang membuat group-commit
meng-amortisasi kedua syscall — biaya dominan jalur durable. Aman di bawah
durable-ack barrier: tak ada yang durable (dan tak ada ack dirilis) sampai
watermark maju saat `Sync`, jadi record pending yang belum di-Sync tak pernah
teramati.

Segment file ukuran tetap (default **1 GiB**, `%06d.wal`); roll-over mem-flush
batch lebih dulu agar satu record tak pernah melintasi dua segment.

### 9.3 Replay yang aman

`Replay(dir, afterSeq, fn)` membaca record `Seq > afterSeq` berurutan:

- **Kontiguitas wajib:** record pertama harus `afterSeq+1`, lalu naik satu-satu.
  Ada lubang → `ErrSeqGap` → **HALT** (jangan menebak).
- **Torn tail:** record tak lengkap / CRC buruk **di ujung segment terakhir** =
  belum durable → berhenti bersih (bukan error).
- **CRC buruk sebelum ujung** = `ErrCorrupt`.

---

## 10. Snapshot & Recovery

### 10.1 Snapshot: lima section dalam satu container WAL

`internal/market/snapshot.go`. Snapshot men-*drain* engine ke batas command,
men-`Sync` journal, lalu menyerialisasi state ke lima section:

| Section | Isi |
|---|---|
| `secHeader` | versi format, scale, layout market→asset |
| `secLedger` | balance, fee terakumulasi, reservasi |
| `secOpenMap` | order pending (`Core.open`) |
| `secBooks` | order book tiap market |
| `secStops` | tabel stop tiap market |

`Snapshotter` (`snapshotter.go`) memicu snapshot berdasarkan **hitungan command**
(`everyN`) atau **interval waktu** (`interval`), menyimpan file `%020d.snap`
(zero-padded `Seq` → urutan leksikal = numerik), dan men-GC sisakan `retainK`
terbaru.

### 10.2 Recovery: snapshot + replay tail

```mermaid
flowchart TB
    START(["Recover(walDir, snapDir)"]) --> SNAP{"snapshot<br/>terbaru ada?"}
    SNAP -->|"ya"| RESTORE["Restore(): muat 5 section<br/>validasi header (versi/scale/market)<br/>rebuild ledger+open+books+stops<br/>selfCheck() invariant"]
    SNAP -->|"tidak / korup"| FRESH["NewEngine() dari nol<br/>(afterSeq = 0)"]
    RESTORE -->|"gagal"| FRESH
    RESTORE -->|"sukses @ Seq S"| REPLAY
    FRESH --> REPLAY["wal.Replay(afterSeq):<br/>tiap record → DecodeCommand →<br/>ApplyJournaled (Seq asli)"]
    REPLAY --> SET["SetSeq(maxSeq)<br/>(watermark untuk lanjut kontigu)"]
    SET --> EN["EnableStops()<br/>(stop suppression OFF, mode live)"]
    EN --> DONE(["engine siap live"])
```

Kunci: selama replay, `SuppressStops = true` agar aktivasi stop (yang **sudah**
ada di log) tidak ter-trigger ganda. Setelah replay selesai, `EnableStops()`
mengembalikan re-injeksi untuk mode live. Snapshot yang korup tidak fatal —
engine *fail-over* ke replay WAL penuh dari `Seq 0` (dicatat di log).

---

## 11. Siklus Hidup Sebuah Order (End-to-End)

Menyatukan semuanya — sebuah limit-buy yang sebagian langsung match:

```mermaid
sequenceDiagram
    autonumber
    participant CL as Klien
    participant ING as ingress ring (SPSC)
    participant SEQ as Sequencer
    participant WAL as WAL
    participant LED as Ledger
    participant SH as Market Shard
    participant ACK as Ack buffer

    CL->>ING: Push(Command{NewOrder, Buy, ...})
    SEQ->>ING: pollExternal() (round-robin)
    SEQ->>SEQ: seq++; TsNanos = clock()
    SEQ->>WAL: Append(record)  (buffered)
    SEQ->>LED: OnCommand → Reserve (round UP)
    alt dana cukup
        LED->>SH: Submit(FundedOrder)
        SH->>SH: matchActive() price-time FIFO
        SH-->>SEQ: Fills + Filled
        SEQ->>LED: Settle tiap fill (round DOWN)<br/>urut (AggressorSeq, MatchIndex)
        LED->>LED: Release sisa untuk order selesai
        SEQ->>ACK: ack(Accepted) masuk buffer
    else dana kurang
        LED->>ACK: ack(Rejected, InsufficientFunds)
    end
    SEQ->>WAL: Sync() saat flushCap/idle (group-commit)
    Note over SEQ,ACK: durableSeq maju → Ack ≤ durableSeq dirilis ke klien
```

---

## 12. Strategi Multi-Core: Serial vs Paralel

Inilah inti "menggunakan core yang berbeda-beda". Engine punya **dua topologi**
yang berbagi sequencer + ledger yang sama.

### 12.1 Topologi Serial (v1, default) — BLP klasik

Sequencer, ledger, dan matching berjalan **inline di satu goroutine**. Caller
(test harness atau `cmd/engine`) menggerakkan loop via `Step()`/`Drain()`. Tidak
ada ring perantara ke shard — `Core.OnCommand` memanggil `Shard.Submit`
langsung.

```mermaid
flowchart LR
    ING["ingress ring"] --> SEQ
    REIN["reinject ring"] --> SEQ
    subgraph CORE0["SATU goroutine / SATU core"]
        SEQ["Sequencer"] -->|"inline"| LED["Ledger"]
        LED -->|"inline"| MATCH["Matching semua market"]
        MATCH -.->|"aktivasi stop"| REIN
    end
    SEQ --> WAL[("WAL")]
```

Paling sederhana, paling deterministik, dan untuk 100k TPS **sudah jauh dari
jenuh** (satu shard Go sanggup >1 juta match/detik).

### 12.2 Topologi Paralel — shard market antar-core

`internal/market/parallel.go`. Market dikelompokkan; **tiap grup berjalan di satu
goroutine worker yang di-pin ke core berbeda** (`platform.PinCurrentThread`).
Jalur kontrol (sequencer + ledger) tetap satu penulis dan **memblokir** menunggu
hasil worker lewat sepasang ring request/response.

```mermaid
flowchart TB
    ING["ingress ring"] --> SEQ
    REIN["reinject ring"] --> SEQ
    subgraph CTRL["Goroutine kontrol (1 core)"]
        SEQ["Sequencer"] --> LED["Ledger (single writer)"]
        LED --> RS0["remoteShard 0"]
        LED --> RS1["remoteShard 1"]
    end
    subgraph W0["Worker 0 — core 0 (di-pin)"]
        SH0["Shard BTC/USDT<br/>book + matching"]
    end
    subgraph W1["Worker 1 — core 1 (di-pin)"]
        SH1["Shard ETH/USDT"]
        SH2["Shard SOL/USDT"]
    end
    RS0 -->|"reqs ring"| W0
    W0 -->|"resps ring"| RS0
    RS1 -->|"reqs ring"| W1
    W1 -->|"resps ring"| RS1
    W0 -.->|"aktivasi stop"| REIN
    W1 -.->|"aktivasi stop"| REIN
```

**Cara worker bekerja** (`worker.run`):

```go
for !w.stop.Load() {
    var req wreq
    if !w.reqs.Pop(&req) { runtime.Gosched(); continue } // busy-poll + yield
    sh := w.shards[req.market]
    var resp wresp
    switch req.kind {
    case reqSubmit:
        r := sh.Submit(req.funded)
        // SALIN slice fills/filled — buffer shard dipakai ulang antar-request
        resp.result = r
        resp.acts   = w.coll.drain()  // aktivasi stop dikumpulkan
    case reqCancel:    resp.ok = sh.Cancel(req.id)
    case reqAmend:     resp.ok = sh.AmendDown(req.id, req.qty)
    case reqLastPrice: resp.price, resp.ok = sh.LastPrice()
    }
    for !w.resps.Push(resp) {}        // spin sampai response masuk
}
```

`remoteShard.call` mendorong request lalu **spin** menunggu response, dan
mendorong aktivasi stop ke `reinject`. Karena ledger tetap single-writer dan
hanya satu shard yang menyentuh sebuah book, **tidak ada lock** — pemisahan ke
core hanya untuk paralelisme matching per-market.

> **Pemetaan grup → worker:** `groups [][]MarketID`. Bila `nil`, tiap market
> dapat worker sendiri. Jumlah worker = jumlah grup.

### 12.3 Pinning core & GC

`internal/platform`:

- **GC off saat sesi:** `GCOff()` = `debug.SetGCPercent(-1)`; `GCOn(prev)`
  memulihkan. Menghapus jitter pause GC pada jalur zero-alloc.
- **Pin thread:** `PinCurrentThread(cpu)` → `runtime.LockOSThread()` di Linux
  (binding afinitas `SchedSetaffinity` menyusul saat `golang.org/x/sys`
  di-vendor); **no-op di Darwin** (macOS tak mengekspos API afinitas — mesin dev
  hanya menjalankan tes korektnes di sini).

```mermaid
flowchart LR
    subgraph CPU["Peta core (contoh paralel)"]
        C0["core 0:<br/>goroutine kontrol<br/>(sequencer+ledger), busy"]
        C1["core 1:<br/>worker 0, busy-spin"]
        C2["core 2:<br/>worker 1, busy-spin"]
        C3["core 3:<br/>housekeeping<br/>(WAL fsync, GC manual)"]
    end
```

---

## 13. Performa & Zero-Alloc

Target rancangan (lihat design doc §1, §14, §18):

| Metrik | Target |
|---|---|
| Throughput berkelanjutan | ≥ 100k cmd/detik (cari titik jenuh) |
| Latency internal p50 / p99 / p99.9 | ≤ 2µs / ≤ 10µs / ≤ 50µs |
| Alokasi di hot path | **0 B/op** (diverifikasi `-benchmem`) |

Teknik yang menjaganya:

- **Tipe POD bebas-pointer** (`Command`, `Fill`, `orderNode`) → ditulis langsung
  ke ring & WAL, tanpa marshalling, tanpa scanning GC.
- **Arena pra-alokasi + free-list** untuk order; semua `make`/`reserve` di
  *startup*, hot path nol-alokasi.
- **SPSC ring** lock-free, cache-line padded, entry dipakai ulang.
- **Buffer dipakai ulang** di sequencer (`payloadBuf`) dan matcher (`fills`,
  `filled`).
- **GC off + pin core + busy-spin** saat sesi ukur.

CI menggerbang zero-alloc lewat `*_bench_test.go` pada `internal/spsc`,
`internal/matching`, `internal/balance` (lihat `make bench`). Lihat
[`docs/designs/invariant-fuzz-testing-guide.md`](docs/designs/invariant-fuzz-testing-guide.md)
untuk kontrak korektnes (`INV-*`) yang diuji via property / differential / fuzz.

---

## 14. Peta Paket

```
cmd/engine          # wiring in-process: recovery startup, cadence snapshot, shutdown
cmd/loadtest        # driver beban open-loop + TUI order-book
cmd/throughput      # harness throughput serial
cmd/internal/harness# generator beban + histogram + TUI (dipakai bersama)

internal/types      # POD value types, fixed-point money (MulDiv), codec WAL, filters
internal/spsc       # SPSC ring lock-free (generic + alias konkret)
internal/orderbook  # book per-market: arena + intrusive FIFO + price levels
internal/matching   # price-time matching, 8 perilaku order type, tabel stop
internal/balance    # ledger single-writer (available/reserved), event bertag
internal/sequencer  # otoritas urutan + fan-in MPSC + journaling + durable-ack barrier
internal/wal        # WAL segmented (record+CRC), group-commit, replay
internal/market     # perakitan engine: serial & paralel; Snapshot/Restore/Recover
internal/platform   # GC off, pin core (build-tagged per OS)
pkg/config          # konfigurasi via env (dibaca sekali saat startup)
pkg/logger          # wrapper slog tipis (startup/shutdown saja)

tests/integration   # end-to-end lintas paket (3 market, balance bersama)
tests/property      # invariant + determinisme + rapid state-machine
tests/refmodel      # oracle model referensi untuk differential testing
```

### Diagram dependensi paket (layering)

```mermaid
flowchart TB
    types["internal/types"]
    spsc["internal/spsc"]
    ob["internal/orderbook"]
    match["internal/matching"]
    bal["internal/balance"]
    seq["internal/sequencer"]
    wal["internal/wal"]
    market["internal/market"]
    cfg["pkg/config"]

    types --> ob --> match
    types --> spsc
    types --> bal
    types --> wal
    spsc --> seq
    wal --> seq
    match --> market
    bal --> market
    seq --> market
    spsc --> market
    cfg --> market
```

Aturan layering (dijaga oleh house rules): `types ← orderbook ← matching ←
market`; `balance`, `sequencer`, `wal` dirakit oleh `market`. `internal/` privat;
`pkg/` satu-satunya permukaan publik.

---

> **Ringkasan satu kalimat:** sebuah Business Logic Processor deterministik —
> sequencer (urutan + journal + durable-ack), ledger single-writer, dan market
> shard penghasil-fill — yang dihubungkan SPSC ring bergaya Disruptor, ditulis
> ke WAL untuk replay byte-identik, dan dapat di-shard ke core berbeda tanpa
> mengorbankan determinisme.
