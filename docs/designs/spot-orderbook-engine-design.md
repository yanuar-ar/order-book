# Spot Order Book Engine — Desain Arsitektur (Go)

> Mesin order book *spot* multi-market, event-sourced, deterministik, zero-alloc.
> **Topologi:** market di-*shard*, balance *bersama*. **Order types:** lengkap.
> **Target:** 100.000 transaksi/detik berkelanjutan dengan headroom besar.
> Dokumen ini ditulis sebagai spesifikasi implementasi untuk dibangun bertahap di Claude Code.

---

## 1. Tujuan & Batasan

| Aspek | Keputusan |
|---|---|
| Jenis pasar | Spot only (tidak ada margin/leverage/posisi) |
| Topologi | 3 *market shard* independen + 1 *balance authority* bersama |
| Order types | Limit, Market, Stop, Stop-Limit, IOC, FOK, Post-Only, Iceberg |
| Prioritas matching | Price-Time (FIFO) |
| Durabilitas | WAL lokal (mmap, append, CRC, group-commit) + snapshot + replay |
| Ketersediaan (HA) | Cluster ganjil (3/5 node), replikasi log via Raft, quorum-commit, failover otomatis (§16) |
| Backup / DR | Arsip WAL + snapshot ke object storage, PITR, verifikasi restore (§17) |
| Antarmuka | In-process engine + benchmark harness (belum ada network gateway/Onload di v1) |
| Angka | Fixed-point integer (`int64`), **bukan** float |
| Runtime | Go modern, hot path zero-alloc, GC off saat sesi, arena off-heap |

### Definisi "transaksi"
Satu **command** masuk = satu transaksi: `NewOrder`, `Cancel`, `Amend`, `Deposit`, atau `Withdraw`.
Target 100k/detik = 1 command tiap 10µs. Satu *shard* matching di Go sanggup >1 juta match/detik (sub-mikrodetik per match), jadi 100k total adalah ~10% kapasitas satu shard — **headroom sangat besar**. Desain ini dirancang agar batasannya adalah korektnes & testability, bukan throughput.

### Target performa (untuk divalidasi, bukan jaminan)
| Metrik | Target |
|---|---|
| Throughput berkelanjutan | ≥ 100k cmd/detik (cari titik maksimum saat benchmark) |
| Latency internal p50 | ≤ 2µs |
| Latency internal p99 | ≤ 10µs |
| Latency internal p99.9 | ≤ 50µs |
| Alokasi di hot path | **0 byte/op** (diverifikasi via `-benchmem`) |

---

## 2. Arsitektur Tingkat Tinggi

```
                          ┌───────────────────────────┐
  Bench / Ingress  ──MPSC─▶│  SEQUENCER  (1 goroutine) │
  (banyak produsen)        │  + WAL (mmap, append)     │
                           │  - assign Seq global + Ts │
                           │  - journal SEMUA command  │
                           │  - urut settlement balik  │
                           └───────────┬───────────────┘
                                       │ command ber-Seq (SPSC per tujuan)
                                       ▼
                           ┌───────────────────────────┐
                           │   BALANCE AUTHORITY        │  ← BERSAMA semua market
                           │   (1 goroutine, ledger)    │
                           │   available / reserved     │
                           └───────────┬───────────────┘
                          funded order │ (SPSC ke market yang tepat)
            ┌─────────────────────────┼─────────────────────────┐
            ▼                         ▼                          ▼
   ┌──────────────────┐     ┌──────────────────┐     ┌──────────────────┐
   │ MARKET SHARD 0   │     │ MARKET SHARD 1   │     │ MARKET SHARD 2   │
   │ BTC/USDT         │     │ ETH/USDT         │     │ SOL/USDT         │
   │ order book+match │     │ order book+match │     │ order book+match │
   └────────┬─────────┘     └────────┬─────────┘     └────────┬─────────┘
            │ Fill (SPSC)            │                        │
            └────────────────────────┼────────────────────────┘
                                     ▼
                          (Fill kembali ke SEQUENCER untuk diberi urutan
                           deterministik, lalu disetel di BALANCE)
                                     │
                                     ▼
                           ┌───────────────────────────┐
                           │  PUBLISHER (1 goroutine)   │
                           │  ack, trade, market data → │
                           │  downstream (async)        │
                           └───────────────────────────┘
```

### Komponen
1. **Ingress / Bench** — sumber command. Banyak produsen → MPSC ke sequencer (lihat §5 untuk pola fan-in).
2. **Sequencer (+WAL)** — satu goroutine. Penulis tunggal urutan global. Memberi `Seq` monotonik + timestamp, men-*journal* setiap command, lalu meneruskan ke balance. Juga menerima `Fill` dari market shard dan memberinya posisi urutan deterministik sebelum settlement.
3. **Balance Authority** — satu goroutine, ledger `available/reserved` bersama untuk semua market. Reservasi saat order diterima, settlement saat fill, release saat cancel. *Single writer* → tidak ada race.
4. **Market Shard ×3** — satu goroutine per market. Memiliki order book + logika matching + semua order types. Tidak menyentuh balance secara langsung; hanya menghasilkan `Fill`.
5. **Publisher** — menyiarkan ack/trade/market data ke downstream secara asinkron (di luar jalur kritis).

---

## 3. Kontrak Determinisme (inti korektnes)

Seluruh sistem adalah **state machine deterministik** di atas satu log event terurut. Aturan yang menjamin replay menghasilkan state byte-identik:

1. **Satu sumber urutan.** Hanya sequencer yang memberi `Seq`. Tidak ada komponen lain mengarang urutan.
2. **Timestamp di-capture sekali** di sequencer (`TsNanos`) dan ditanam ke event. Replay memakai nilai tersimpan — **tidak pernah** membaca jam lagi.
3. **Tiap komponen = fungsi murni dari aliran input terurutnya.** Tanpa randomness, tanpa I/O eksternal, tanpa wall-clock di dalam logika.
4. **Antar-komponen lewat SPSC FIFO** → urutan terjaga di tiap link.
5. **Fill diurutkan deterministik** oleh kunci `(aggressor_seq, match_index)` — properti data, bukan waktu kedatangan. Sequencer menerapkan settlement dalam urutan kunci ini. Karena itu, walau matching berjalan di goroutine terpisah, hasilnya punya urutan tetap yang sama saat replay.
6. **WAL hanya mencatat command eksternal** (Model "journal input"). Saat recovery, matcher menghasilkan ulang Fill yang identik karena (3)+(5). Balance menerapkan reservasi pada `Seq` command dan settlement pada `(aggressor_seq, match_index)` → urutan identik.

> **Konsekuensi penting (benar secara bisnis):** kamu tak bisa menjual aset yang fill-nya belum disetel pada posisi `Seq` lebih awal. Ini deterministik (bergantung urutan `Seq`, bukan timing), dan memang perilaku spot yang benar.

---

## 4. Tipe Data Inti

`internal/types/types.go` — semua POD, **tanpa pointer** (ramah GC & WAL).

```go
package types

type (
	Price     int64  // fixed-point, diskalakan PriceScale
	Qty       int64  // fixed-point, diskalakan QtyScale
	AccountID uint64
	OrderID   uint64
	MarketID  uint32
	AssetID   uint32
	Seq       uint64
)

const (
	PriceScale = 100_000_000 // 1e8 → 8 desimal
	QtyScale   = 100_000_000
)

type Side uint8
const (
	Buy Side = iota
	Sell
)

type OrderType uint8
const (
	Limit OrderType = iota
	Market
	Stop      // stop-market: trigger → market
	StopLimit // trigger → limit
)

type TIF uint8
const (
	GTC TIF = iota // good-till-cancel
	IOC            // immediate-or-cancel
	FOK            // fill-or-kill
)

type Flags uint16
const (
	FlagPostOnly Flags = 1 << iota
	FlagIceberg
)

type CmdType uint8
const (
	CmdNewOrder CmdType = iota
	CmdCancel
	CmdAmend
	CmdDeposit
	CmdWithdraw
)

// Command: ukuran tetap, tanpa pointer → bisa ditulis langsung ke WAL & ring.
type Command struct {
	Seq        Seq
	TsNanos    int64
	Type       CmdType
	Market     MarketID
	Account    AccountID
	OrderID    OrderID
	Side       Side
	OrdType    OrderType
	Tif        TIF
	Flags      Flags
	Price      Price // harga limit / harga trigger stop
	StopPrice  Price
	Qty        Qty
	DisplayQty Qty     // porsi tampak iceberg
	Asset      AssetID // untuk deposit/withdraw
	Amount     int64   // untuk deposit/withdraw
}

// Fill: hasil satu eksekusi antara dua order.
type Fill struct {
	AggressorSeq Seq // Seq order agresor (untuk urutan deterministik)
	MatchIndex   uint32
	Market       MarketID
	Price        Price
	Qty          Qty
	BuyOrder     OrderID
	SellOrder    OrderID
	BuyAccount   AccountID
	SellAccount  AccountID
}
```

> **Catatan uang:** semua aritmetika harga×qty dilakukan di `int64`/`int128` ter-skala. Sediakan helper `quote = price * qty / QtyScale` dengan penanganan pembulatan eksplisit (round-down untuk reservasi, agar tidak pernah under-reserve). Uji pembulatan di unit test.

---

## 5. SPSC Ring Buffer

`internal/spsc/ring.go` — lock-free, cache-line padded, kapasitas power-of-two, simpan *by value*.

```go
package spsc

import "sync/atomic"

type Ring[T any] struct {
	_    [64]byte
	head atomic.Uint64 // dibaca consumer
	_    [56]byte       // padding: pisahkan ke cache line berbeda
	tail atomic.Uint64  // dibaca producer
	_    [56]byte
	buf  []T
	mask uint64
}

// capacity HARUS power of two.
func New[T any](capacity uint64) *Ring[T] {
	return &Ring[T]{buf: make([]T, capacity), mask: capacity - 1}
}

func (r *Ring[T]) Push(v T) bool {
	t := r.tail.Load()
	if t-r.head.Load() >= uint64(len(r.buf)) {
		return false // penuh
	}
	r.buf[t&r.mask] = v
	r.tail.Store(t + 1) // publish
	return true
}

func (r *Ring[T]) Pop(out *T) bool {
	h := r.head.Load()
	if h == r.tail.Load() {
		return false // kosong
	}
	*out = r.buf[h&r.mask]
	r.head.Store(h + 1)
	return true
}
```

- Atomik Go bersifat **sequentially consistent** (tidak ada acquire/release eksplisit) — benar, sedikit lebih mahal dari C++.
- Generics menambah overhead kecil; untuk link terpanas pertimbangkan versi tipe-konkret (mis. `RingCommand`, `RingFill`).
- **Dipakai di tiap link:** sequencer→balance, balance→market[i], market[i]→sequencer (fill), sequencer/balance→publisher.

### Fan-in MPSC ke sequencer
Banyak produsen → satu sequencer. **Jangan** pakai satu queue MPSC ber-CAS. Pola yang dipakai: **tiap produsen punya SPSC sendiri, sequencer poll semua bergiliran (round-robin)**. Setiap link tetap SPSC (tercepat), sequencer yang melakukan penggabungan + penetapan `Seq`.

---

## 6. Sequencer + Write-Ahead Log

`internal/sequencer` & `internal/wal`.

### 6.1 Loop sequencer (deterministik)
```
loop:
  1. tiriskan ring FILL dari semua market shard:
        untuk tiap Fill (urut by AggressorSeq, MatchIndex):
            journal sebagai SettlementEvent (opsional di Model-input: cukup derivasi)
            kirim ke Balance untuk settle
  2. poll ring command dari produsen (round-robin), ambil satu:
        assign Seq++  dan TsNanos = now()   (now hanya di sini)
        tulis Command ke WAL (group-commit)
        kirim ke Balance untuk reservasi/routing
  (busy-spin)
```
> Aturan "tiriskan fill sebelum ambil command baru" memberi interleaving deterministik **bersama** kunci `(aggressor_seq, match_index)`.

### 6.2 Format record WAL
Framing **length-prefixed + CRC**, header natural-aligned (jangan `pack(1)`):

```go
type RecordHeader struct {
	Seq     uint64
	TsNanos int64
	Length  uint32 // total record (header + payload)
	CRC32   uint32 // checksum payload
	Type    uint16
	Flags   uint16
	_       uint32 // padding → 32 byte
}
```

### 6.3 Penulisan
- WAL = file di-`mmap` (`unix.Mmap`), append = `copy` ke `[]byte` page cache (kecepatan RAM).
- **Publish** record dengan menyimpan *commit offset* atomik (`atomic.Uint64`) — pembaca/replay hanya baca s/d offset itu.
- **Segmen** file ukuran tetap (mis. 1 GiB). Pre-fault segmen berikutnya agar roll-over tidak stall.
- **Durabilitas v1:** *group-commit* `fdatasync` per N record atau per X µs (amortisasi). Ack dilepas setelah batch durable. (*Persist sebelum lepas output.*)

### 6.4 Snapshot
- Periodik: serialisasi state (balance + semua order book) ke disk, catat `Seq` terakhir yang diterapkan.
- v1 sederhana: *pause-and-snapshot* di antara batch (aman karena single-writer). Fase berikut: *shadow* consumer untuk snapshot tanpa pause.

### 6.5 Recovery
```
1. muat snapshot terakhir → state @ Seq = S
2. buka WAL, seek ke record pertama dengan Seq > S
3. untuk tiap record:
     verifikasi CRC  (buruk di ujung → torn write → truncate, stop)
     cek Seq kontigu (gap → HALT, jangan tebak)
     terapkan command lewat topologi yang sama (deterministik)
4. lanjut live dari Seq berikutnya
```

---

## 7. Balance Authority (bersama)

`internal/balance`. Satu goroutine, *single writer* → konsisten lintas market tanpa lock.

```go
type Balance struct {
	Available int64
	Reserved  int64
}

// key gabungan account|asset → Balance
type key struct {
	Acct  AccountID
	Asset AssetID
}

type Ledger struct {
	bal map[key]Balance // di-make dengan kapasitas besar agar tak rehash
}
```

### Operasi
| Event | Aksi |
|---|---|
| `Deposit` | `Available += amount` |
| `Withdraw` | cek `Available ≥ amount` → `Available -= amount` (else reject) |
| `NewOrder` (buy) | hitung `cost = price*qty (+fee)`; cek `Available ≥ cost` → `Available -= cost; Reserved += cost`; else **reject**. Lalu kirim *funded order* ke market shard |
| `NewOrder` (sell) | reservasi `qty` aset basis (analog) |
| `Fill` | sisi beli: `Reserved -= quote_terpakai`, aset basis `Available += qty`; sisi jual: `Reserved -= qty`, quote `Available += proceeds` |
| `Cancel` (acked oleh market) | lepas sisa: `Reserved -= sisa; Available += sisa` |

- **Reservasi di muka** mencegah double-spend lintas market (USDT yang sama dipakai di BTC/USDT & ETH/USDT).
- **Post-Only/Iceberg/Stop** tetap memakai reservasi yang sama; perbedaannya hanya di logika matching (§9).
- Determinisme: reservasi diterapkan pada `Seq` command; settlement pada `(aggressor_seq, match_index)`.
- *Scale-out (fase berikut):* shard ledger per-hash akun; v1 cukup satu goroutine (100k TPS jauh di bawah kapasitas).

---

## 8. Order Book (per market shard)

`internal/orderbook`. Arena `[]orderNode` + index `uint32` (bukan pointer) → cache-friendly & nol scanning GC. Intrusive doubly-linked list untuk FIFO per level.

```go
const NilIdx uint32 = 0xFFFFFFFF

type orderNode struct {
	id         OrderID
	account    AccountID
	price      Price
	remaining  Qty   // sisa qty total
	display    Qty   // sisa qty tampak (iceberg)
	hidden     Qty   // sisa qty tersembunyi (iceberg)
	side       Side
	typ        OrderType
	tif        TIF
	flags      Flags
	next, prev uint32 // FIFO dalam level (index arena)
}

type level struct {
	head, tail uint32 // order tertua → termuda
	totalQty   Qty    // hanya qty tampak (untuk market data)
}

type Book struct {
	market   MarketID
	arena    []orderNode      // pre-allocated
	free     []uint32         // free-list slot daur ulang
	bids     []level          // price ladder (index by tick)
	asks     []level
	idIndex  map[OrderID]uint32
	bestBid  Price
	bestAsk  Price
	stops    []stopOrder      // tabel stop menunggu trigger (§9)
}
```

- **Price ladder** (array per tick) untuk akses level O(1) di sekitar touch; gunakan rentang harga terikat per market. Untuk harga jauh/sparse, fallback peta.
- `idIndex` pakai map non-pointer, di-`make` besar (tanpa rehash); kandidat ganti ke array bila OrderID padat.
- Semua `make`/`reserve` di **startup**; hot path **nol-alokasi**.

---

## 9. Matching Engine & Order Types

`internal/matching`. Inti: **price-time priority**. Order agresor menyapu level lawan dari harga terbaik, FIFO dalam level, sampai habis atau harga tak lagi cocok.

### Inti loop (pseudocode)
```
match(aggressor):
  matchIdx = 0
  while aggressor.remaining > 0 and ada level lawan yang harganya cocok:
      lvl = best level lawan
      for resting in lvl (FIFO):
          x = min(aggressor.remaining, resting.display)
          emit Fill{AggressorSeq, matchIdx++, price=resting.price, qty=x, ...}
          aggressor.remaining -= x
          kurangi resting (lihat Iceberg untuk replenish)
          if resting habis: hapus dari book, free slot
          if aggressor.remaining == 0: break
  sisakan/aksi sesuai tipe (di bawah)
```

### Semantik tiap tipe
| Tipe | Perilaku |
|---|---|
| **Limit (GTC)** | match sebisanya; sisa **rest** di book pada `price`. |
| **Market** | match terhadap book tanpa batas harga; sisa **dibatalkan** (tak pernah rest). Reservasi pakai estimasi konservatif / batas slippage. |
| **IOC** | match sebisanya **segera**; sisa langsung **cancel** (tak rest). |
| **FOK** | **cek dulu** apakah seluruh qty bisa terisi segera; bila tidak → **reject total** tanpa eksekusi apa pun. Bila ya → eksekusi penuh. |
| **Post-Only** | harus jadi *maker*. Bila order akan langsung *cross* (match) → **reject** (atau reprice, pilih satu kebijakan). Bila tidak → rest. |
| **Iceberg** | hanya `display` tampak di book. Saat `display` habis tapi `hidden > 0`: isi ulang `display` dari `hidden` dan **re-queue di belakang** level (kehilangan prioritas waktu — perilaku standar). |
| **Stop / Stop-Limit** | tidak masuk book aktif. Disimpan di tabel `stops`. **Trigger** saat *last trade price* market melewati `StopPrice`. Saat trigger: Stop → menjadi Market; Stop-Limit → menjadi Limit pada `Price`. Order hasil trigger diberi `Seq` baru lewat sequencer (deterministik). |

### Mekanisme trigger Stop
Setiap `Fill` memperbarui `lastPrice` market. Setelah memproses order agresor, scan `stops` untuk yang ter-trigger oleh `lastPrice` (buy-stop bila `last ≥ stopPrice`, sell-stop bila `last ≤ stopPrice`), aktifkan dalam urutan deterministik (mis. by `Seq` order stop). Aktivasi = kirim order baru ke sequencer → kembali masuk pipeline normal. **Hindari** rekursi tak terbatas dengan memproses trigger sebagai event baru, bukan inline.

---

## 10. Protokol Antar-Komponen

Pesan yang mengalir di ring (semua POD):

| Pesan | Dari → Ke | Isi |
|---|---|---|
| `Command` | Ingress → Sequencer → Balance | order/cancel/deposit/withdraw ber-`Seq` |
| `FundedOrder` | Balance → Market[i] | command yang lolos reservasi |
| `Reject` | Balance/Market → Publisher | alasan (dana kurang, post-only cross, FOK gagal) |
| `Fill` | Market[i] → Sequencer | hasil eksekusi (`AggressorSeq`, `MatchIndex`, dua akun) |
| `Settlement` | Sequencer → Balance | derivasi dari Fill, urut deterministik |
| `Ack` / `Trade` / `BookUpdate` | mana saja → Publisher | output ke downstream |

Routing market: `marketID → index shard`. Balance tahu market dari `Command.Market`, mengirim `FundedOrder` ke ring market yang tepat.

---

## 11. Layout Paket (untuk scaffold di Claude Code)

```
spot-engine/
├── cmd/
│   ├── engine/main.go        # wiring semua komponen + pin core
│   └── bench/main.go         # load harness (generator + pengukur)
├── internal/
│   ├── types/                # Command, Fill, Price/Qty, enum  (§4)
│   ├── spsc/                 # ring buffer generic + tipe-konkret (§5)
│   ├── wal/                  # mmap append, record, CRC, segmen, replay (§6)
│   ├── sequencer/            # loop urutan + fan-in MPSC (§6)
│   ├── balance/              # ledger available/reserved (§7)
│   ├── orderbook/            # arena, level, price ladder (§8)
│   ├── matching/             # price-time + semua order types (§9)
│   ├── market/              # shard: bungkus orderbook+matching+ring
│   └── platform/             # LockOSThread, SchedSetaffinity, Mlockall, mmap hugepage
├── test/
│   └── integration/          # skenario 3 market (§13.2)
└── bench/                     # hasil & skrip pengukuran (§14)
```

### Urutan build yang disarankan (umpankan ke Claude Code bertahap)
1. `types` → 2. `spsc` (+ unit test) → 3. `orderbook` (+ test) → 4. `matching` (+ test tiap order type) → 5. `balance` (+ test) → 6. `wal` (+ test replay) → 7. `sequencer` (+ test determinisme) → 8. `market` + wiring `cmd/engine` → 9. `test/integration` → 10. `cmd/bench` + pengukuran.

---

## 12. Strategi Threading & Core

| Goroutine | Pin | Sifat |
|---|---|---|
| Sequencer | core terisolasi | busy-spin |
| Balance | core terisolasi | busy-spin |
| Market shard ×3 | core terisolasi (CCD sama, L3 bersama) | busy-spin |
| Publisher | core terisolasi / housekeeping | busy-spin atau batched |
| WAL fsync | housekeeping | berkala (group-commit) |

- `runtime.LockOSThread()` + `unix.SchedSetaffinity` per goroutine hot.
- `debug.SetGCPercent(-1)` saat sesi + `SetMemoryLimit` sebagai jaring; `runtime.GC()` manual saat idle.
- `GOMAXPROCS` = jumlah core hot + housekeeping. Confine proses via `cpuset`.
- Arena order book & ring → **off-heap `unix.Mmap`** (kontrol NUMA + nol scanning GC).
- Catatan jujur: *async preemption* Go menyisakan jitter kecil yang tak bisa dihapus total — ukur (§14), dan untuk 100k TPS ini aman.

---

## 13. Strategi Pengujian

### 13.1 Unit test (per paket)
- **spsc:** push/pop benar; deteksi penuh/kosong; wraparound melewati `mask`; (uji konkuren producer/consumer dengan `-race` di mode test).
- **orderbook:** insert/cancel/amend; integritas FIFO per level; best bid/ask terupdate; daur ulang free-list; konsistensi `idIndex`.
- **matching (table-driven, satu tabel per tipe):**
  - Limit: partial fill lalu rest; full fill.
  - Market: sweep banyak level; sisa dibatalkan.
  - IOC: sisa di-cancel; tak ada yang rest.
  - FOK: cukup → fill penuh; kurang → **reject total, book tak berubah**.
  - Post-Only: akan cross → reject; tidak cross → rest.
  - Iceberg: display habis → replenish dari hidden → re-queue di belakang level.
  - Stop/Stop-Limit: trigger pada lastPrice melewati ambang; aktivasi jadi order baru.
- **balance:** reserve/settle/release; dana kurang → reject; deposit/withdraw; **pembulatan** (round-down reservasi, tak pernah under-reserve); tak pernah negatif.
- **wal:** append→read round-trip; CRC mendeteksi korupsi; torn write di ujung → truncate; replay membangun state identik.
- **sequencer:** `Seq` monotonik & kontigu; interleaving fill vs command deterministik.

### 13.2 Integration test — 3 market dengan balance bersama
Spin up engine penuh: market **BTC/USDT, ETH/USDT, SOL/USDT**; aset bersama: BTC, ETH, SOL, **USDT**.

Skenario wajib:
1. **Konsistensi balance lintas market.** Satu akun deposit USDT, pasang order beli di BTC/USDT **dan** ETH/USDT yang totalnya melebihi saldo → order kedua/ketiga **ditolak** dengan benar (USDT yang sama tak bisa dipakai dua kali). Inilah uji "sharded + balance bersama".
2. **Matching dasar lintas semua market** berjalan paralel; trade benar.
3. **Order types end-to-end:** FOK reject, Post-Only reject, Iceberg replenish, Stop trigger oleh pergerakan harga, IOC sisa-cancel.
4. **Cancel & release:** reserved kembali ke available.
5. **Recovery determinisme (krusial):** jalankan beban acak → snapshot + WAL → "matikan" engine → **replay** → bandingkan state akhir (semua book + seluruh ledger) **byte-identik** dengan sebelum mati. Ini menguji seluruh kontrak determinisme.
6. **Invarian global** dicek di akhir:
   - Tidak ada balance negatif.
   - `Reserved` tiap akun == jumlah dana terkunci semua order terbukanya.
   - **Konservasi nilai:** total aset (available+reserved) per aset konstan terhadap deposit/withdraw bersih (tidak ada uang tercipta/hilang dari matching).
   - Book konsisten: tak ada bid ≥ ask yang tersisa (post-match), linked-list utuh.

### 13.3 Property / fuzz test
Generator order acak (campuran tipe, banyak akun, banyak market) → jalankan ribuan langkah → assert seluruh invarian §13.2.6 di setiap langkah. Jalankan juga **dua kali dengan seed sama → output identik** (determinisme).

---

## 14. Pengukuran Performa

### 14.1 Metrik
- **Throughput:** command ter-ack per detik (steady-state) dan **maksimum** (dorong sampai jenuh).
- **Latency:** p50/p99/p99.9/max — **end-to-end** (ingress→ack) dan **per-stage** (waktu di sequencer, balance, matching). Fokus ke **tail**.
- **Jitter:** histogram durasi iterasi loop matching (deteksi pause GC / preemption).
- **Alokasi:** 0 B/op di hot path.

### 14.2 Alat & teknik (Go)
- **HdrHistogram:** `github.com/HdrHistogram/hdrhistogram-go` untuk distribusi latency presisi.
- **Timestamp:** `time.Now()`/`time.Since` memakai monotonic clock via VDSO (~20–40ns). Untuk per-stage sub-µs, sematkan `TsNanos` ingress di `Command` dan ukur selisih di ack. Untuk micro-bench paling presisi, pertimbangkan cycle counter (asm/`runtime.nanotime`).
- **Micro-bench (`testing.B`, `-benchmem`):** `Ring.Push/Pop`, satu `match()` pada book terisi, satu operasi reserve/settle balance. Verifikasi **0 allocs/op**.
- **Macro / load harness (`cmd/bench`):**
  - Generator goroutine memproduksi `Command` (rate target ATAU secepat mungkin untuk cari maks), distribusi akun & campuran order type bisa dikonfigurasi.
  - **Warmup** (buang N detik pertama), lalu ukur **steady-state**.
  - Tandai latency: ingress stamp → ack stamp → rekam ke HdrHistogram.
  - Tangani backpressure: bila ring penuh, hitung & laporkan (jangan blok pengukuran diam-diam).
- **Verifikasi GC off:** jalankan dengan `GODEBUG=gctrace=1` → pastikan tak ada siklus GC selama sesi ukur. Cek `runtime.ReadMemStats` (NumGC tidak naik).

### 14.3 Prosedur uji 100k TPS
1. Set `GOMAXPROCS`, pin goroutine, `SetGCPercent(-1)`.
2. Generator pada 100k cmd/detik selama ≥60 detik setelah warmup.
3. Catat throughput tercapai + p50/p99/p99.9/max + histogram jitter.
4. Naikkan rate bertahap (200k, 500k, 1M…) sampai p99 melewati target → laporkan **titik jenuh** dan **headroom** di atas 100k.
5. Bandingkan dengan target §1. Bila tail meleset → profil (`pprof`), cari alokasi tersembunyi / kontensi ring / jitter preemption.

### 14.4 "Bagus" itu seperti apa
100k TPS harus tercapai dengan utilisasi rendah dan tail jauh di bawah target. Bila p99 internal > 10µs pada 100k TPS, kemungkinan ada alokasi di hot path, ring undersized, atau goroutine hot tidak ter-pin — periksa tiga itu lebih dulu.

---

## 15. Catatan Build & Run

- **Go:** versi modern (generics, typed atomics).
- **Flags:** `-trimpath`; pertimbangkan PGO (Go 1.21+) setelah profil beban realistis.
- **Env/runtime:** `SetGCPercent(-1)`, `SetMemoryLimit`, `GOMAXPROCS` sesuai peta core; `taskset`/`cpuset` untuk confine proses; hugepage opsional via `MAP_HUGETLB`.
- **Jangan** pakai channel Go di hot path (pakai `spsc`); **jangan** alokasi di hot path; **jangan** `interface{}`/`any` dinamis atau `fmt` di hot path.

---

## 16. High Availability: Replikasi, Failover, Redundancy

### 16.1 Ide inti
Engine sudah berupa **Replicated State Machine (RSM)**: state = fungsi deterministik dari satu log command terurut (§3). Maka HA tidak perlu mereplikasi *state* — cukup mereplikasi **log**-nya ke quorum, lalu tiap node memutar log yang sama → state identik. Inilah kenapa determinisme dibangun sejak awal: ia adalah prasyarat HA, bukan tambahan.

### 16.2 Topologi cluster
- **Jumlah node ganjil**: 3 (tahan 1 kegagalan) atau 5 (tahan 2). Ganjil agar ada **quorum mayoritas**.
- Satu node **leader** (memberi Seq, melayani gateway), sisanya **follower (hot standby)** yang terus memutar committed log → state in-memory mereka selalu hangat.
- **Penempatan**: untuk hot path, taruh semua node di satu zona latensi-rendah (RTT antar-node kecil = commit cepat). Replikasi ke region jauh dilakukan **asinkron** untuk DR (§17), **bukan** bagian dari quorum sinkron (WAN RTT akan meracuni latency commit).
- Gateway **stateless & redundan**, mengarah ke leader saat ini (info kepemimpinan dari Raft).

### 16.3 Pemetaan komponen → Raft
| Konsep di §1–§9 | Padanan Raft |
|---|---|
| WAL (log command terurut) | **Raft log** (dipersist di tiap node sebagai mmap segmen) |
| `Seq` global | Raft log **index** |
| "durable" (group-commit) | Raft **commit index** (quorum sudah punya entry) |
| State machine (balance + 3 book) | Raft **FSM** (`Apply(entry)`) — sama di semua node |
| Recovery/replay | Raft **snapshot + log replay** (mekanisme sama, §6.5) |

Library Go: **`go.etcd.io/raft`** (teruji, dipakai etcd) atau **`dragonboat`** (multi-group Raft, performa tinggi). `hashicorp/raft` juga opsi. (Padanan latensi-ekstrem di dunia C++/Java adalah Aeron Cluster — di luar scope Go v1.)

### 16.4 Alur HA — "match on commit"
```
Command → Leader → Raft.Propose(cmd)
       → (replikasi ke follower) → quorum durable → COMMIT
       → Apply(cmd) di SEMUA node: reservasi balance → match → settle
       → Leader lepas Ack ke klien
```
> **Aturan korektnes diperluas:** *jangan lepas output sebelum input ter-commit di quorum.* State machine hanya maju pada entry yang **sudah commit** → semua node match identik. Matching tetap µs; yang menambah latency-ack hanyalah **RTT commit Raft**.

**Trade-off latency (jujur):** ack durable kini = latensi match (µs) **+** RTT quorum + fsync follower (puluhan–ratusan µs dalam satu DC). Untuk menjaga throughput tinggi meski ada RTT, Raft **mem-batch & pipeline** banyak command per ronde (mis. 100 cmd/ronde → 1000 ronde/detik cukup untuk 100k TPS). Karena itu definisikan **dua SLO terpisah** (lihat §18): *internal match latency* (µs) dan *durable-ack latency* (termasuk replikasi).

*Opsi optimasi (lanjutan):* speculative match sebelum commit, ack ditahan sampai commit. Lebih rumit (hasil spekulatif dibuang bila leader gagal sebelum commit) — **tidak** untuk v1.

### 16.5 Urutan failover
1. Follower kehilangan heartbeat leader → timeout pemilihan (Raft).
2. **Pemilihan leader** Raft: hanya kandidat dengan log **paling mutakhir** yang bisa menang (jaminan keamanan Raft → tak ada committed entry hilang).
3. Leader baru menerapkan committed entry yang belum di-apply (sedikit, karena hot standby sudah hampir sinkron).
4. Leader baru lanjut memberi Seq dari commit index terakhir.
5. Gateway mendeteksi leader baru → arahkan command ke sana.
6. Command in-flight yang **belum** di-ack saat leader lama jatuh → klien **retry** (lihat idempotensi).

Target failover realistis: **detik tunggal** (election timeout + apply sisa). Hot standby membuat tak perlu replay panjang.

### 16.6 Anti split-brain & idempotensi
- **Split-brain dicegah quorum + term/epoch Raft.** Leader lama yang terpartisi tak bisa quorum → tak bisa commit; entry-nya yang belum commit dibuang saat bergabung kembali. Fencing via **term** (tulisan dengan term basi ditolak).
- **Idempotensi/dedup (wajib untuk retry aman):** tiap command bawa `ClientReqID` unik (mis. `(account, client_seq)`). Engine menyimpan dedup-set per akun (window bergulir) → command yang ter-retry **tidak diterapkan dua kali**. Ini memberi semantik *exactly-once* efektif menyeberangi failover. Tambah field `ClientReqID uint64` di `Command` (§4).

### 16.7 Redundancy di luar konsensus
- **Per market via multi-group Raft (opsional, lanjutan):** dengan `dragonboat`, tiap market bisa jadi Raft group sendiri → failover & beban independen per market. v1 cukup **satu group untuk seluruh engine** (lebih sederhana; 100k TPS jauh di bawah batas).
- **Hardware**: dual NIC (bonding), PSU redundan, disk NVMe RAID/mirror untuk log lokal.
- **Tidak ada SPOF**: leader bisa diganti; gateway redundan; WAL ada di setiap node.

### 16.8 Tambahan layout paket
```
internal/cluster/        # integrasi Raft: FSM Apply(), Propose(), snapshot hook
internal/cluster/fsm.go  # bungkus sequencer+balance+market sebagai Raft FSM
internal/dedup/          # ClientReqID dedup per akun
cmd/engine/main.go       # mode single-node ATAU cluster (flag --peers)
```

---

## 17. Backup & Disaster Recovery

### 17.1 Replikasi ≠ backup
Replikasi (§16) menjaga **ketersediaan** terhadap kegagalan node — tapi replika **menyalin korupsi logis dengan setia** (command buruk, deploy bug, salah hapus operator). Backup melindungi terhadap itu, plus menyediakan **arsip kepatuhan** (regulator menuntut retensi bertahun-tahun). Keduanya wajib, beda tujuan.

### 17.2 Komponen backup
- **Arsip WAL kontinu:** tiap segmen WAL yang sudah "disegel" dikirim ke **object storage** (S3/GCS/MinIO) — immutable & berversi. Ini fondasi PITR.
- **Snapshot periodik** (akhir sesi + tiap N menit) ke object storage, tiap snapshot ditandai **`Seq` watermark**-nya.
- **Salinan offsite/DR** lintas region (asinkron, di luar quorum sinkron) untuk bencana situs.
- **Enkripsi at-rest** untuk semua backup (kepatuhan).

### 17.3 Point-in-Time Recovery (PITR)
```
restore(target_seq):
  1. ambil snapshot terbesar dengan watermark S ≤ target_seq
  2. muat snapshot
  3. ambil segmen WAL terarsip, replay entry (S, target_seq]
  4. state = persis kondisi pada target_seq
```
Berguna untuk forensik, resolusi sengketa ("seperti apa book pada waktu T"), dan pemulihan dari korupsi logis (restore ke titik **sebelum** command buruk).

### 17.4 Retensi
| Tier | Isi | Simpan |
|---|---|---|
| Panas (lokal/quorum) | WAL aktif + state in-memory | berjalan |
| Hangat (object storage) | Segmen WAL + snapshot harian | hari–minggu |
| Dingin/arsip (immutable) | WAL + snapshot historis, terenkripsi | tahun (sesuai regulator) |

### 17.5 Verifikasi backup (kritis)
> *Backup yang belum pernah diuji-restore = bukan backup.*

Job otomatis berkala: restore snapshot + replay WAL terarsip ke **sandbox terisolasi**, lalu jalankan **invarian §13.2.6** (tak ada balance negatif, konservasi nilai, integritas book). Bila gagal → alarm. Jadikan ini bagian CI/operasional, bukan harapan.

---

## 18. Load Test 100k TPS (konkret)

Melengkapi §14 (filosofi metrik). Section ini = harness yang bisa langsung dibangun di `cmd/bench`.

### 18.1 Tujuan & kriteria lulus
- **Throughput:** ≥ 100k cmd/detik berkelanjutan ≥ 60 detik (lalu cari titik jenuh).
- **Dua SLO latency:**
  - *Internal match latency* (ingress→hasil match, tanpa replikasi): p99 ≤ 10µs.
  - *Durable-ack latency* (termasuk quorum-commit bila HA aktif): p99 ≤ target operasional (mis. ≤ 1ms intra-DC) — tetapkan sesuai infrastruktur.
- **Alokasi:** 0 B/op di hot path. **GC:** `NumGC` tidak naik selama sesi ukur.

### 18.2 Hindari Coordinated Omission (penting!)
Pakai **open-loop**: jadwalkan command ke-`i` pada waktu **niat** `t_i = start + i/rate`, dan ukur latency dari `t_i` (bukan dari waktu kirim aktual). Bila sistem tersendat, latency niat akan membengkak dengan benar — *closed-loop* (kirim berikutnya hanya setelah ack) **menyembunyikan** tail. HdrHistogram menyediakan koreksi via `RecordCorrectedValue(latency, expectedInterval)`.

### 18.3 Model beban (realistis)
- **Market berbobot:** BTC/USDT 50%, ETH/USDT 30%, SOL/USDT 20%.
- **Campuran order type:** Limit/GTC ~70%, Market 10%, IOC 5%, FOK 5%, Post-Only 5%, Cancel 3%, Stop+Iceberg 2%.
- **Akun:** pool ~10.000 akun, semuanya **di-deposit** USDT+aset basis di fase setup.
- **Seed likuiditas:** isi book tiap market dengan N order limit istirahat di sekitar mid **sebelum** ukur — agar matching benar-benar terjadi (bukan semua rest).
- **Distribusi harga:** di sekitar mid sehingga sebagian menyilang (trade) & sebagian rest.

### 18.4 Fase
1. **Setup:** deposit semua akun, seed likuiditas, pin core, `SetGCPercent(-1)`.
2. **Warmup:** ~10 detik, hasil dibuang.
3. **Steady-state:** ≥ 60 detik pada 100k TPS → rekam.
4. **Ramp:** naikkan 200k → 500k → 1M … sampai p99 (internal) lewat target → laporkan **titik jenuh** & **headroom**.

### 18.5 Generator open-loop (sketsa Go)
```go
// cmd/bench/load.go (inti, disederhanakan)
func runLoad(in *spsc.Ring[types.Command], rate int, dur time.Duration,
	gen *Generator, hist *hdrhistogram.Histogram) {

	interval := time.Duration(int64(time.Second) / int64(rate)) // jeda antar-cmd
	start := time.Now()
	var i int64
	for {
		intended := start.Add(time.Duration(i) * interval)
		now := time.Now()
		if d := intended.Sub(now); d > 0 {
			busyWait(d) // spin halus; hindari time.Sleep yang kasar di rate tinggi
		}
		cmd := gen.Next()               // pilih market/tipe/akun/harga sesuai model
		cmd.ClientTsNanos = intended.UnixNano() // stempel waktu NIAT untuk koreksi CO
		for !in.Push(cmd) {             // backpressure: ring penuh
			atomic.AddInt64(&gen.backpressure, 1)
		}
		i++
		if time.Since(start) >= dur {
			break
		}
	}
}

// Konsumen ack (goroutine terpisah) — koreksi coordinated omission:
func onAck(ack types.Ack, hist *hdrhistogram.Histogram, expectedIntervalNanos int64) {
	lat := time.Now().UnixNano() - ack.ClientTsNanos
	hist.RecordCorrectedValue(lat, expectedIntervalNanos) // <-- kunci anti-CO
}
```
> Tambah field bench-only `ClientTsNanos int64` di `Command`/`Ack` untuk korelasi latency. `busyWait` = spin pendek (cek `time.Now()`), karena `time.Sleep` terlalu kasar untuk pacing 100k/detik (10µs/cmd).

### 18.6 Yang diukur & dilaporkan
- Throughput tercapai (ack/detik) vs target.
- Histogram: **p50/p99/p99.9/p99.99/max** untuk kedua SLO.
- **Backpressure count** (push gagal) — bila > 0 berarti ring/engine tak mengejar; besarkan ring atau periksa hot path.
- **Jitter loop matching:** histogram durasi iterasi (deteksi pause).
- **GC:** `runtime.ReadMemStats` sebelum/sesudah → `NumGC` sama; jalankan juga dengan `GODEBUG=gctrace=1`.
- **Alokasi:** micro-bench terpisah `testing.B -benchmem` pada jalur `Push/match/reserve` → 0 allocs/op.

### 18.7 Diagnosis bila meleset
| Gejala | Periksa |
|---|---|
| Throughput < 100k | ring undersized, goroutine tak ter-pin, generator sendiri jadi bottleneck (jalankan multi-generator) |
| p99 internal > 10µs | alokasi tersembunyi (`pprof alloc`), false sharing (cek padding ring), preemption (pin + `GOMAXPROCS`) |
| Tail (p99.9+) berduri | GC menyala (pastikan `SetGCPercent(-1)` & zero-alloc), async preemption, IRQ di core hot |
| durable-ack p99 tinggi | RTT quorum/fsync follower → besarkan batch Raft, dekatkan node, NVMe lebih cepat |

---

## Lampiran A — Ringkasan keputusan kunci
1. Spot, reservasi `available/reserved` di muka → cegah double-spend lintas market.
2. Market di-shard (3 goroutine), **balance authority bersama** (single writer) → konsisten tanpa lock.
3. Determinisme: satu log terurut (WAL), fill diurut `(aggressor_seq, match_index)`, replay = identik.
4. Hot path zero-alloc + GC off + pin core; arena off-heap.
5. 100k TPS = headroom besar; fokus desain ke korektnes, determinisme, dan testability.
6. **HA = RSM di atas Raft**: replikasi log (bukan state) ke quorum ganjil, hot standby, failover otomatis dalam detik, match-on-commit. Determinisme adalah prasyaratnya (§16).
7. **Backup ≠ replikasi**: arsip WAL+snapshot ke object storage, PITR, retensi bertingkat, verifikasi restore otomatis (§17).
8. **Load test open-loop** dengan koreksi coordinated omission; dua SLO terpisah (internal match µs vs durable-ack termasuk replikasi) (§18).
