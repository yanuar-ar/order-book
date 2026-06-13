# Invariant & Fuzz Testing — Panduan & Best Practices

> Referensi korektnes untuk **Spot Order Book Engine** (lihat `spot-orderbook-engine-design.md`).
> Tujuan: mendefinisikan **setiap invarian yang tak boleh dilanggar** secara presisi, plus metodologi fuzz/property test untuk mencari pelanggarannya. Untuk uang, satu bug = kerugian nyata, jadi dokumen ini sengaja **selengkap-lengkapnya**.

## Cara pakai dokumen ini di Claude Code
1. Tiap invarian punya **ID stabil** (`INV-<kategori>-<nn>`). Jadikan nama test, mis. `TestInvBal02_NoSpendBeyondBalance`.
2. Implementasikan **satu fungsi pemeriksa murni** `CheckAllInvariants(state) error` yang menjalankan semua invarian §3 dan mengembalikan pelanggaran pertama beserta konteks.
3. Bangun **differential fuzz harness** (§4): engine asli vs *reference model*, jalankan stream command acak yang sama, bandingkan output + state, dan panggil `CheckAllInvariants` **setelah setiap command**.
4. Setiap bug yang ditemukan → simpan **seed** sebagai corpus regresi permanen (§5). Jangan pernah dihapus.

---

## 1. Filosofi: tiga lapis pertahanan

| Lapis | Apa | Menangkap |
|---|---|---|
| **Unit test (table-driven)** | contoh konkret per skenario/tipe order | bug logika yang sudah kamu pikirkan |
| **Invariant check** | properti yang HARUS selalu benar, dicek tiap langkah | state ilegal yang tak kamu duga |
| **Differential model (oracle)** | bandingkan dengan implementasi lambat-tapi-jelas-benar | bug *semantik* (hasil match salah tapi state tetap "valid") |

Invarian saja tak cukup: sebuah engine bisa menjaga semua invarian struktural tapi tetap **mencocokkan order yang salah**. Karena itu *reference model* (§4.1) wajib — ia satu-satunya oracle yang menangkap "jawaban salah tapi rapi".

---

## 2. Best Practices Fuzz / Property Testing

1. **Cek invarian setelah SETIAP operasi**, bukan hanya di akhir. Ini menunjuk persis command mana yang merusak state.
2. **Model-based / stateful testing.** Jalankan engine + *reference model* pada stream command identik; assert output & state cocok di tiap langkah.
3. **Seed deterministik + log seed.** Tiap run mencatat seed-nya; kegagalan harus 100% reproducible dengan memutar ulang seed.
4. **Shrinking.** Saat gagal, perkecil urutan command ke reproducer minimal (library `rapid`/`gopter` melakukannya otomatis).
5. **Corpus regresi permanen.** Tiap bug → seed/`testdata/fuzz` tetap. Bug yang pernah ada tak boleh kembali diam-diam.
6. **Coverage-guided + structured.** Gabungkan: Go native `go test -fuzz` (mutasi byte coverage-guided atas stream command terserialisasi) **dan** `pgregory.net/rapid` (state-machine terstruktur).
7. **Generator adversarial, bukan cuma valid.** Sengaja produksi kasus tepi (§6): saldo pas-pasan, FOK kurang satu unit, harga/qty di batas overflow, iceberg display>hidden, stop yang trigger seketika, cancel ID acak, duplikasi `ClientReqID`, arus lintas-market dengan akun yang sama.
8. **Differential antar-build untuk determinisme.** Dua proses engine, stream sama → state byte-identik.
9. **Metamorphic properties** (§3.H). Mis. "batalkan semua order terbuka → tiap akun kembali ke saldo awal dikurangi trade ter-realisasi".
10. **Generator harus deterministik.** Tanpa wall-clock, tanpa iterasi `map` yang bocor ke urutan command (urutan map Go acak!). Selalu pakai PRNG ber-seed.
11. **Uji modelnya juga.** Validasi *reference model* dengan contoh tulisan-tangan, supaya oracle yang buggy tak menutupi bug engine.
12. **Jalankan `-race` berkala** pada jalur ring konkuren (walau hot path single-thread).
13. **Soak panjang di CI malam** (menit–jam); fuzz pendek di tiap PR.

---

## 3. Taksonomi Invarian (assertable)

Notasi: cek berlaku **setelah setiap command selesai diproses** kecuali disebut lain. `quote(p,q)` = fungsi konversi harga×qty ke unit aset quote dengan pembulatan terdefinisi (lihat §3.B).

### A. Balance & Konservasi (paling kritis)

- **INV-BAL-01 — Non-negatif.** Untuk tiap `(account, asset)`: `available ≥ 0` **dan** `reserved ≥ 0`.
- **INV-BAL-02 — Tak bisa beli melebihi saldo.** `NewOrder` hanya diterima jika reservasi yang dibutuhkan ≤ `available` saat penerimaan. Reservasi tak pernah membuat `available` negatif. *(Invarian "gak bisa beli melebihi balance".)*
- **INV-BAL-03 — Reserved = jumlah terkunci order terbuka.** Untuk tiap `(account, asset)`: `reserved == Σ locked(order)` atas semua order terbuka akun itu di aset itu. (Buy limit mengunci quote = `quote(limitPrice, remaining)`; sell mengunci base = `remaining`.) **Cross-check ledger ↔ book paling kuat.**
- **INV-BAL-04 — Konservasi nilai global per aset.** Untuk tiap aset `X`: `Σ_acct (available_X + reserved_X) + feeAccount_X == totalDeposit_X − totalWithdraw_X`. Matching **tak pernah** menciptakan/menghancurkan unit.
- **INV-BAL-05 — Trade zero-sum per fill.** Pada satu fill (harga `p`, qty `q`): base yang keluar dari penjual == base masuk ke pembeli == `q`; quote keluar dari pembeli == quote masuk ke penjual + fee ke fee account. Per aset, jumlah perubahan antar-pihak = 0.
- **INV-BAL-06 — Settled ≤ reserved per order.** Total quote yang benar-benar dibelanjakan order buy ≤ yang direservasi untuknya. Kelebihan reservasi dilepas saat selesai/cancel.
- **INV-BAL-07 — Release-on-cancel tepat.** Cancel order bersisa `r`: `reserved −= locked(r)`, `available += locked(r)`. Tak lebih, tak kurang.
- **INV-BAL-08 — Withdraw aman.** `Withdraw(a)` diterima hanya jika `a ≤ available` (reserved **tidak** bisa ditarik).
- **INV-BAL-09 — Tak ada double-spend lintas market.** Dana yang direservasi untuk order di market A tidak tersedia untuk order di market B. `Σ reservasi semua market untuk (account,asset) ≤ deposit bersih`. Wajib di-fuzz dengan akun yang sama di banyak market.
- **INV-BAL-10 — Konservasi fee.** `Σ fee didebet == Σ fee dikredit ke fee account` (sudah tercakup INV-BAL-04 bila fee account dimasukkan).

### B. Aritmetika / Fixed-Point

- **INV-ARI-01 — Tanpa overflow.** `price * qty` dihitung tanpa overflow `int64`. Gunakan perantara 128-bit (`math/bits.Mul64`) atau batasi input. **Fuzz dekat `int64` max.**
- **INV-ARI-02 — Reservasi membulat KE ATAS (ceil).** Reservasi buy ≥ biaya eksak → tak pernah under-reserve. (`quote_reserve = ceil(...)`.)
- **INV-ARI-03 — Settlement membulat terdefinisi & ≤ reservasi.** `settled = quote(p,q)` dengan arah pembulatan terdefinisi; `settled ≤ reserved`; `released = reserved − settled` dikembalikan ke `available`.
- **INV-ARI-04 — Pembulatan tak membocorkan nilai.** Residu pembulatan harus ke tempat terakun (dust/fee account) sehingga INV-BAL-04 tetap berlaku. Tetapkan arah pembulatan satu kebijakan; assert konservasi tetap utuh.
- **INV-ARI-05 — Konsistensi skala.** Harga selalu `PriceScale`, qty `QtyScale`; `quote()` dipakai **identik** di estimasi reservasi & settlement (tak ada drift); tak ada campur skala/non-skala.
- **INV-ARI-06 — Remaining monoton turun.** `remaining` order hanya berkurang via fill (tak pernah naik). Pengecualian: replenish iceberg = refresh terkendali, dan `display+hidden` tetap hanya berkurang.
- **INV-ARI-07 — Tick & lot.** `price` kelipatan `tickSize`; `qty` kelipatan `lotSize`. Order melanggar → **ditolak**, bukan dibulatkan diam-diam. Fuzz input off-tick/off-lot.

### C. Struktur Order Book

- **INV-OB-01 — Tak ada book menyilang saat istirahat.** Setelah aggressor selesai diproses: tak ada resting bid ≥ resting ask. `bestBid < bestAsk` bila keduanya ada.
- **INV-OB-02 — Urutan harga.** Bid menurun (terbaik = tertinggi), ask menaik (terbaik = terendah). Cache `bestBid/bestAsk` benar.
- **INV-OB-03 — Integritas FIFO list.** Per level: `head.prev == NIL`, `tail.next == NIL`, tiap node `next/prev` saling-balik, tak ada siklus, panjang traversal hingga == jumlah node.
- **INV-OB-04 — Total level = jumlah.** `level.totalQty == Σ displayQty` order di level itu (hanya qty tampak).
- **INV-OB-05 — Integritas arena/free-list.** Tiap slot arena tepat satu dari: (a) terjangkau dari suatu level, atau (b) di free-list. Tak ada slot keduanya/tak-satu pun. **Tak ada kebocoran slot** sepanjang run panjang.
- **INV-OB-06 — Konsistensi idIndex.** `idIndex` = persis himpunan order terbuka; tiap ID → slot benar; tak ada entri basi/hilang.
- **INV-OB-07 — Keunikan OrderID.** Tak ada dua order terbuka ber-ID sama.
- **INV-OB-08 — Prioritas waktu.** Urutan dalam level == urutan insert (seq/ts monoton sepanjang list).
- **INV-OB-09 — Batas remaining.** Untuk tiap order resting: `0 < remaining ≤ originalQty`. Order ber-`remaining == 0` harus dihapus, bukan istirahat.

### D. Semantik Matching (umum)

- **INV-MAT-01 — Harga terbaik dulu.** Fill terjadi pada harga lawan terbaik sebelum harga lebih buruk. Tak ada order berharga lebih buruk terisi selagi yang lebih baik masih bersisa.
- **INV-MAT-02 — Waktu dalam harga.** Di antara resting seharga sama, yang paling awal (head FIFO) terisi dulu.
- **INV-MAT-03 — Eksekusi di harga maker.** Tiap fill pada harga **resting (maker)**, bukan harga aggressor. (Price improvement untuk taker.)
- **INV-MAT-04 — Limit dihormati.** Buy aggressor tak pernah fill di harga > limit; sell aggressor tak pernah < limit.
- **INV-MAT-05 — Konservasi qty per fill.** `fill.qty == min(aggressor.remaining, resting.display)` dan `> 0`.
- **INV-MAT-06 — Batas total aggressor.** `Σ fill.qty` untuk satu aggressor ≤ qty yang diajukan.
- **INV-MAT-07 — Self-Trade Prevention (opsional).** Jika STP aktif: order akun tak match resting milik akun itu sendiri; kebijakan STP (cancel-newest/oldest/both) terdefinisi & di-assert.
- **INV-MAT-08 — Hasil deterministik.** Stream command terurut sama → fill identik (harga/qty/urutan) & book+balance identik.

### E. Per Order Type (lengkap)

**GTC (Limit, good-till-cancel)**
- **INV-GTC-01** — Setelah proses: terisi penuh (lenyap dari book) **atau** sisa istirahat **tepat di limit price**-nya.
- **INV-GTC-02** — Resting GTC bertahan melintasi command berikutnya sampai terisi/dibatalkan.
- **INV-GTC-03** — Reservasi cocok dengan sisa resting (INV-BAL-03).

**IOC (Immediate-or-Cancel)**
- **INV-IOC-01** — **Tak pernah istirahat**: setelah proses, IOC tidak ada di book.
- **INV-IOC-02** — Mengisi qty yang tersedia seketika pada harga layak; sisa dibatalkan.
- **INV-IOC-03** — Reservasi sisa yang dibatalkan dilepas penuh (INV-BAL-07).
- **INV-IOC-04** — Fill qty ∈ [0, diajukan].

**FOK (Fill-or-Kill)**
- **INV-FOK-01** — **All-or-nothing**: hasil = terisi penuh **atau** nol fill.
- **INV-FOK-02** — **Rollback atomik**: bila tak bisa terisi penuh, book & SEMUA balance **byte-identik** dengan sebelum FOK diproses (tanpa efek samping parsial). *Cek fillability SEBELUM memutasi apa pun.*
- **INV-FOK-03** — Cek fillability hanya pakai likuiditas pada harga layak (hormati limit).
- **INV-FOK-04** — Tak pernah istirahat.

**Market**
- **INV-MKT-01** — Tak pernah istirahat; menyapu book lintas harga memburuk sampai terisi/book habis.
- **INV-MKT-02** — Sisa (book habis) dibatalkan.
- **INV-MKT-03** — Reservasi menutup worst-case (atau dibatasi slippage); `settled ≤ reserved`; sisa dilepas.
- **INV-MKT-04** — Tanpa batas harga (fill di harga resting berapa pun).
- **INV-MKT-05** — Book lawan kosong → nol fill, order dibatalkan (bukan istirahat), reservasi dilepas.

**Post-Only**
- **INV-PO-01** — **Tak pernah ambil likuiditas**: nol taker fill. Bila akan menyilang (buy `price ≥ bestAsk` atau sell `price ≤ bestBid`) → **ditolak** (kebijakan) tanpa perubahan apa pun.
- **INV-PO-02** — Bila tak menyilang → istirahat seperti GTC.
- **INV-PO-03** — Post-only yang ditolak meninggalkan book & balance tak berubah.

**Stop / Stop-Limit**
- **INV-STP-01** — Tidak aktif sebelum trigger: tak pernah di book aktif, tak pernah match, sampai kondisi trigger terpenuhi.
- **INV-STP-02** — Kondisi trigger: buy-stop saat `lastTradePrice ≥ stopPrice`; sell-stop saat `lastTradePrice ≤ stopPrice`. (Harga acuan = last trade; definisikan eksplisit.)
- **INV-STP-03** — Saat trigger: Stop → perilaku Market; Stop-Limit → Limit di limit price.
- **INV-STP-04** — Trigger dievaluasi deterministik setelah tiap trade yang menggerakkan `lastPrice`; urutan banyak stop yang trigger bersamaan deterministik (by seq).
- **INV-STP-05** — Order ter-trigger masuk kembali sebagai order aktif baru (seq baru) — **tanpa rekursi inline / tanpa cascade tak hingga**.
- **INV-STP-06** — Reservasi ditahan sejak penerimaan selama periode tidak-aktif.

**Iceberg**
- **INV-ICE-01** — Hanya `display` tampak: `level.totalQty` hanya menghitung porsi display; hidden tak terlihat di market data.
- **INV-ICE-02** — Replenish saat display habis: `display = min(displaySize, hidden)`, `hidden -= display`.
- **INV-ICE-03** — **Prioritas waktu hilang saat replenish**: slice baru di-antre di **belakang** level (di belakang order seharga sama).
- **INV-ICE-04** — Total tereksekusi ≤ total asli (`display+hidden`); `Σ fill` lawan iceberg == porsi terisi.
- **INV-ICE-05** — Reservasi menutup `hidden+display` sampai tereksekusi/dibatalkan.
- **INV-ICE-06** — Dihapus saat `display` & `hidden` keduanya 0.

**Cancel**
- **INV-CXL-01** — Setelah cancel: order lenyap dari book + idIndex.
- **INV-CXL-02** — Melepas tepat reservasi sisa belum-terisi (INV-BAL-07).
- **INV-CXL-03** — **Idempotent**: cancel ID tak dikenal/sudah-hilang = no-op (return not-found), tanpa double-release.
- **INV-CXL-04** — Cancel order terisi-sebagian: hanya sisa dilepas; porsi tersettle tak tersentuh.

**Amend / Modify**
- **INV-AMD-01** — Hanya kurangi qty → pertahankan prioritas waktu; reservasi dikurangi.
- **INV-AMD-02** — Naikkan qty / ubah harga → kehilangan prioritas (semantik cancel+insert). Definisikan & assert.
- **INV-AMD-03** — Reservasi selalu cocok dengan sisa resting setelah amend.
- **INV-AMD-04** — Amend ke qty < sudah-terisi → diperlakukan cancel (atau ditolak) per kebijakan.

### F. Determinisme & Recovery

- **INV-DET-01** — Replay WAL sama → state byte-identik (semua book + ledger). Jalankan dua kali, bandingkan.
- **INV-DET-02** — Snapshot+replay setara: `(load snapshot@S + replay (S,N])` == `replay [0,N]`.
- **INV-DET-03** — Crash-consistency: setelah truncate WAL di torn-tail, recovery menghasilkan state yang memenuhi semua invarian A–E.
- **INV-DET-04** — Urutan fill deterministik by `(aggressorSeq, matchIndex)`.

### G. Idempotensi

- **INV-IDM-01** — `ClientReqID` duplikat → diterapkan **paling banyak sekali**; state sama dengan penerapan tunggal.
- **INV-IDM-02** — Retry cancel/order menyeberangi failover tidak menerapkan dua kali.

### H. Metamorphic (properti turunan)

- **INV-MET-01 — Cancel-all kembali ke baseline.** Batalkan semua order terbuka → tiap `(account,asset).reserved == 0` dan `available == deposit_bersih ± trade_terealisasi`.
- **INV-MET-02 — Book kosong ⇒ reserved nol.** Bila semua order suatu akun lenyap (terisi/dibatalkan), `reserved` akun itu = 0 di semua aset.
- **INV-MET-03 — Permutasi non-bersilang komutatif.** Order yang tak pernah saling-match (mis. semua di sisi & harga berbeda) menghasilkan book sama tanpa peduli urutan kedatangan relatifnya.
- **INV-MET-04 — Replay subset.** Memproses `[0,k]` lalu `[k+1,N]` == memproses `[0,N]` (sejalan INV-DET).

---

## 4. Desain Fuzz Harness (Go)

### 4.1 Reference model (oracle)
Implementasi **lambat tapi jelas benar**: order book pakai slice ter-sort + ledger `map`. Tak peduli performa; peduli *kebenaran yang gampang dibaca*. Engine asli & model menerima command sama; bandingkan output + state.

```go
type Model interface {
    Apply(cmd types.Command) []types.Event // fills/acks/rejects, terurut
    Snapshot() ModelState                  // balances + ringkasan book per market
}
```

### 4.2 Differential loop (inti)
```go
func runDifferential(t *testing.T, seed int64, steps int) {
    rng := rand.New(rand.NewSource(seed))
    eng := engine.NewForTest(markets, assets) // engine asli
    mod := refmodel.New(markets, assets)      // oracle
    gen := NewGenerator(rng, markets, accounts)

    for i := 0; i < steps; i++ {
        cmd := gen.Next()

        gotEng := eng.Apply(cmd)
        gotMod := mod.Apply(cmd)

        // 1) Output harus cocok (menangkap bug SEMANTIK)
        if !eventsEqual(gotEng, gotMod) {
            t.Fatalf("seed=%d step=%d cmd=%+v\n eng=%v\n mod=%v", seed, i, cmd, gotEng, gotMod)
        }
        // 2) Snapshot state harus cocok
        if !stateEqual(eng.Snapshot(), mod.Snapshot()) {
            t.Fatalf("STATE DIVERGE seed=%d step=%d cmd=%+v", seed, i, cmd)
        }
        // 3) Invarian engine asli harus utuh — setiap langkah
        if err := CheckAllInvariants(eng.StateView()); err != nil {
            t.Fatalf("INVARIANT seed=%d step=%d cmd=%+v: %v", seed, i, cmd, err)
        }
    }
}
```

### 4.3 Pemeriksa invarian (pure)
```go
// Mengembalikan pelanggaran PERTAMA dengan konteks; nil bila bersih.
func CheckAllInvariants(s StateView) error {
    if err := checkBalanceNonNegative(s); err != nil { return wrap("INV-BAL-01", err) }
    if err := checkReservedEqualsLocked(s); err != nil { return wrap("INV-BAL-03", err) }
    if err := checkConservation(s); err != nil { return wrap("INV-BAL-04", err) }
    if err := checkNoCrossedBook(s); err != nil { return wrap("INV-OB-01", err) }
    if err := checkLevelTotals(s); err != nil { return wrap("INV-OB-04", err) }
    if err := checkArenaFreelist(s); err != nil { return wrap("INV-OB-05", err) }
    if err := checkIdIndex(s); err != nil { return wrap("INV-OB-06", err) }
    if err := checkRemainingBounds(s); err != nil { return wrap("INV-OB-09", err) }
    // ... seluruh §3 ...
    return nil
}
```
Contoh dua cek paling bernilai:
```go
// INV-BAL-03: reserved == Σ locked(open orders)
func checkReservedEqualsLocked(s StateView) error {
    want := map[Key]int64{}
    for _, mkt := range s.Markets {
        for _, o := range mkt.OpenOrders() {
            want[lockKey(o)] += lockedAmount(o) // quote utk buy, base utk sell
        }
    }
    for k, bal := range s.Ledger {
        if bal.Reserved != want[k] {
            return fmt.Errorf("acct=%v asset=%v reserved=%d wantLocked=%d",
                k.Acct, k.Asset, bal.Reserved, want[k])
        }
    }
    return nil
}

// INV-BAL-04: konservasi per aset (termasuk fee account)
func checkConservation(s StateView) error {
    for asset, expected := range s.NetDeposits { // deposit − withdraw
        var sum int64
        for _, bal := range s.LedgerForAsset(asset) {
            sum += bal.Available + bal.Reserved
        }
        sum += s.FeeAccount(asset)
        if sum != expected {
            return fmt.Errorf("asset=%v total=%d expected=%d (LEAK=%d)",
                asset, sum, expected, sum-expected)
        }
    }
    return nil
}
```

### 4.4 Go native fuzz (coverage-guided)
Encode stream command sebagai `[]byte` → decode → jalankan differential. Fuzzer memutasi byte secara coverage-guided.
```go
func FuzzEngine(f *testing.F) {
    f.Add([]byte{}) // + seed corpus dari testdata/fuzz
    f.Fuzz(func(t *testing.T, data []byte) {
        cmds := decodeCommands(data) // []byte → []Command (tahan input rusak)
        eng := engine.NewForTest(markets, assets)
        mod := refmodel.New(markets, assets)
        for i, cmd := range cmds {
            ge, gm := eng.Apply(cmd), mod.Apply(cmd)
            if !eventsEqual(ge, gm) { t.Fatalf("step=%d diverge", i) }
            if err := CheckAllInvariants(eng.StateView()); err != nil { t.Fatal(err) }
        }
    })
}
```
Jalankan: `go test -run=^$ -fuzz=FuzzEngine -fuzztime=10m ./...`

### 4.5 Stateful property (rapid)
`pgregory.net/rapid` cocok untuk state-machine + shrinking otomatis:
```go
func TestEngineStateMachine(t *testing.T) {
    rapid.Check(t, func(rt *rapid.T) {
        eng := engine.NewForTest(markets, assets)
        mod := refmodel.New(markets, assets)
        rt.Repeat(map[string]func(*rapid.T){
            "newOrder": func(rt *rapid.T) { applyBoth(rt, eng, mod, genNewOrder(rt)) },
            "cancel":   func(rt *rapid.T) { applyBoth(rt, eng, mod, genCancel(rt)) },
            "deposit":  func(rt *rapid.T) { applyBoth(rt, eng, mod, genDeposit(rt)) },
            "": func(rt *rapid.T) { // dipanggil tiap langkah
                if err := CheckAllInvariants(eng.StateView()); err != nil { rt.Fatal(err) }
            },
        })
    })
}
```

---

## 5. Corpus Regresi & Reproduksibilitas
- **Log seed** di setiap run differential (§4.2). Kegagalan → `go test ... -seed=<n>` memutar ulang.
- **Simpan reproducer minimal** (hasil shrinking) sebagai test eksplisit di `testdata/fuzz/FuzzEngine/` (Go native otomatis menyimpan input gagal).
- **Aturan:** tiap bug yang diperbaiki **wajib** menambah satu seed/corpus regresi. CI menjalankannya selamanya.

---

## 6. Skenario Adversarial yang WAJIB di-seed eksplisit

Bug bersembunyi di tepi. Generator harus sering memproduksi (dan ada seed eksplisit untuk):

| Target | Skenario |
|---|---|
| INV-BAL-02 | order yang **pas** menghabiskan available; order yang **kurang 1 unit**; dua order yang gabungannya melebihi saldo |
| INV-BAL-09 | akun sama, order beli di 3 market berbeda yang total quote-nya > saldo USDT |
| INV-ARI-01 | `price`/`qty` dekat `int64` max; `price*qty` yang overflow naif |
| INV-ARI-02/04 | harga/qty yang `quote()`-nya tidak habis dibagi (uji pembulatan & dust) |
| INV-FOK-02 | FOK yang **pas** terisi; FOK **kurang 1 unit** (harus rollback byte-identik) |
| INV-PO-01 | post-only yang **pas menyentuh** bestAsk/bestBid (boundary menyilang) |
| INV-ICE-03 | iceberg dengan banyak order seharga sama → verifikasi kehilangan prioritas saat replenish |
| INV-STP-02/05 | stop yang trigger **seketika** saat diajukan; banyak stop trigger oleh satu trade; stop yang trigger stop lain |
| INV-MAT-02 | ribuan order harga identik (stress FIFO) |
| INV-CXL-03 | cancel ID acak/sudah-dibatalkan/sudah-terisi |
| INV-IDM-01 | `ClientReqID` duplikat berturut & terjeda |
| INV-MKT-05 | market/IOC saat book lawan kosong |

---

## 7. Checklist sebelum dianggap "teruji"

- [ ] `CheckAllInvariants` mengimplementasikan **setiap** ID di §3 dan dipanggil tiap langkah.
- [ ] *Reference model* ada, lulus contoh tulisan-tangan, dipakai di differential loop.
- [ ] Unit test table-driven untuk **tiap order type** (§3.E) — kasus normal + tepi.
- [ ] Go native fuzz (`-fuzz`) berjalan di CI (pendek di PR, panjang di malam).
- [ ] Property/state-machine test (`rapid`) dengan shrinking aktif.
- [ ] Semua skenario adversarial §6 punya seed eksplisit.
- [ ] Tes determinisme: dua run seed sama → byte-identik (INV-DET-01).
- [ ] Tes recovery: snapshot + replay = state identik & invarian utuh (INV-DET-02/03).
- [ ] Tiap bug historis punya seed regresi permanen.
- [ ] Cek aritmetika overflow & pembulatan punya tes terarah (bukan hanya berharap fuzz menemukannya).

---

## Lampiran — Prioritas (jika waktu terbatas)
Urutan dampak tertinggi → terendah untuk uang:
1. **INV-BAL-04** (konservasi) + **INV-BAL-03** (reserved=locked) — menangkap hampir semua bug uang.
2. **INV-BAL-02** (tak bisa beli melebihi saldo) + **INV-ARI-01/02** (overflow/pembulatan).
3. **INV-FOK-02** (rollback atomik) — bug rollback parsial = uang bocor.
4. **INV-OB-01/05** (book tak menyilang, arena tak bocor).
5. **Differential model** — payung yang menangkap sisanya.
