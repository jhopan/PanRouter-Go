### fix(antigravity): blokir akun yang tidak layak, kuota persisten, dan re-verify pra-skip

Tiga mekanisme blokir Antigravity yang belum ada di port Go. Ketiganya menghemat
satu round-trip yang pasti gagal, dan ketiganya fail-open: kegagalan KV atau
kegagalan panggilan kuota tidak pernah menggagalkan request.

**1. Pre-filter eligibility akun (`open-sse/services/antigravityCapCache.js`).**
Google bisa menandai akun `VALIDATION_REQUIRED` — CLI resmi membaca
`ineligibleTiers` dari `loadCodeAssist` lalu **berhenti**, sementara gateway ini
mengabaikan field itu sehingga akun bertanda tetap melayani trafik dan setiap
request jadi round-trip yang sia-sia. Dua sinyal upstream yang kini dibaca:

- `loadCodeAssist` → `ParseAntigravityEligibility` (`ineligibleTiers`; tier yang
  menyebut alasan dipilih, `allowedTiers` saja berarti sehat).
- quota 403 → `ParseAntigravityQuotaEligibility` (`error.details[].reason`,
  `metadata.validation_url`, atau link Help `accounts.google.com`).

`RecordAntigravityEligibility` menyimpan verdict per koneksi di KV scope
`antigravity_ineligible` dan `ShouldSkipAntigravityAccount` melewati akun itu
sebelum request keluar (`connections.go`: jalur pin dan jalur rotasi).
`GetAntigravityIneligibility` menyuplai baris log. TTL **15 menit**
(`AgIneligibleTTL`), cache dibatasi `agIneligibleMaxEntries = 512` dengan
pembersihan entri kedaluwarsa. Verdict sehat **atau tanpa opini** menghapus entri
memori **dan** baris KV tanpa syarat — setelah restart map kosong tetapi barisnya
masih ada, jadi penghapusan bersyarat membuat flag hidup sampai TTL habis (bug
upstream yang diperbaiki di sana). `RestoreAntigravityIneligible` memulihkan saat
boot, membuang baris kedaluwarsa, dan `ClearAntigravityIneligible` dipakai tombol
"check & release".

Baris KV hanya menyimpan `{flagged, flaggedAt (epoch ms), reasonCode}` — **bukan**
`validationUrl`: link verifikasi membawa token lanjutan dan aturan repo
(`internal/providers/antigravity_ban.go`) melarangnya masuk DB/log. URL tetap
hidup di memori untuk dashboard, dan bila body tidak memuat `validation_url`,
`AntigravityVerdictFromErrorBody` mengambil link dari `ExtractAppealURL` (allowlist
host). Ini satu-satunya deviasi dari bentuk baris upstream, dan disengaja.

Verdict dicatat di tempat eligibility ditetapkan: bacaan usage dashboard
(`usage.go`, setara `usage.js`; juga health dari `loadCodeAssist`), quota refresh
chat pada 403, dan 403 media search. Check-and-release memakai verdict yang sama:
selama akun masih bertanda ia menjawab `Still needs verification
(VALIDATION_REQUIRED) — open the verification link, then check again`, dan begitu
provider menyatakan sehat ia memanggil `ClearAntigravityIneligible` lalu melepas
blok — persis alur tombol upstream.

**2. Kuota habis yang persisten (`ag_quota`).** `persistAntigravityExhausted`
hanya menyimpan bucket `remainingPercentage == 0` dengan resetAt **di masa depan**
— entri itu bisa memblokir berhari-hari dan harus bertahan setelah restart.
`removeAntigravityPersistedIfHealthy` menghapus baris begitu tidak ada yang habis,
dan `LoadPersistedAntigravityQuota` memulihkannya saat boot: entri kedaluwarsa
dibuang, baris yang kosong dihapus. Keduanya dipanggil berurutan setelah setiap
refresh (persist dulu, baru cleanup, supaya cleanup tidak menghapus yang baru
ditulis). Strike block 15 menit dan bacaan sehat tetap di memori saja.

**3. Re-verify sebelum skip (`REVALIDATE_COOLDOWN_MS` → `AgRevalidateCooldown = 5
menit`).** Pre-filter melewati akun berdasarkan bucket 0% di cache; skip itu
terjadi **sebelum** request keluar, jadi jalur error — satu-satunya yang pernah
me-refresh cache — tidak pernah jalan. Bucket 0% yang salah karena itu membekukan
akun sampai resetAt-nya sendiri (upstream mengamati 27 jam akun sehat dilewati
padahal bucket upstream 99,55%). `SelectAntigravitySkipsToRevalidate` (murni,
tanpa jaringan/clock sendiri) memilih koneksi yang akan dilewati dan sudah lewat
cooldown; `RevalidateAntigravityExhausted` mencatat timestamp **sebelum** await
sehingga request bersamaan tidak memanggil kuota dua kali, lalu menjalankan satu
panggilan quota per koneksi secara paralel (upstream `Promise.all`) dan menelan
error. Dipanggil dari `getBestConnectionWithContext` sebelum filter ketersediaan,
dengan gate murah (`antigravityAnyExhaustedCached`) agar decode token per baris
hanya terjadi saat memang ada yang dilewati.

**4. Bucket Argon (`gemini_argon_weekly`).** `AntigravityFamilyKey`
(`gemini|palm|gemma|imagen` → `gemini_weekly`, `claude|gpt|opus|sonnet` →
`claude_gpt_weekly`) dan `AntigravityArgonBucket` dipakai di jalur error dan di
keputusan skip. Argon adalah **preferensi**, bukan pengganti: bila akun melaporkan
bucket Argon, bucket itu yang memutuskan (bucket Argon habis tidak pernah
"diselamatkan" kuota Gemini biasa, dan 0% palsu di `gemini_weekly` tidak memblokir
tier Argon yang masih dilayani); tanpa bucket Argon, bucket family yang memutuskan
— fallback plan Ultra. `antigravityDecideQuota` menegakkan urutan upstream
Argon → family → baris per-model (+ sinonim), jadi 429 free-tier tidak lagi
salah dibaca "kuota tidak diketahui" lalu diblok hanya 15 menit. Jalur error kini
memakai `antigravityStrikeOnOptimistic` dengan semantik #4197 yang tidak berubah
(hanya marker kuota eksplisit yang menghitung strike) dan strike dihapus setelah
blok, sama seperti upstream.

**Konstanta.** `AgIneligibleTTL = 15m` · scope `antigravity_ineligible` ·
`agIneligibleMaxEntries = 512` · `AgQuotaScope = "ag_quota"` ·
`AgRevalidateCooldown = 5m` · `agRevalidateMaxEntries = 4096` (entri lebih tua
dari cooldown dipangkas setiap tulis) · `AgArgonBucketKey = "gemini_argon_weekly"`.
Clock dan KV di-inject (`agNow`, `SetAntigravityKVStore`) supaya bisa diuji tanpa
tidur dan tanpa jaringan; wiring boot ada di `internal/app/server.go`.

**Verifikasi.** Tidak ada test yang tidur atau menyentuh jaringan; waktu digerakkan
lewat `agNow`, kuota lewat refresher yang di-inject, storage lewat KV palsu:

- `TestAntigravityIneligibleTTL` — flag masih melewati akun pada 14m, tidak lagi
  setelah 15m+1s.
- `TestAntigravityIneligibleRestoreDropsExpiredRows` — baris kedaluwarsa dibuang
  **dan** dihapus, baris segar dipulihkan (return 1).
- `TestAntigravityIneligibleHealthyVerdictClearsPersistedRowAfterRestart` — flag +
  restart (map dikosongkan) lalu verdict sehat: baris KV hilang, restore
  berikutnya 0; sekaligus memastikan baris tidak memuat URL verifikasi.
- `TestAntigravityIneligibleFailOpenOnKVError` — store error: flag memori tetap
  jalan, restore/load 0, clear tetap bekerja.
- `TestAntigravityIneligibleCacheStaysBounded` — 512 entri kedaluwarsa dipangkas,
  flag hidup selamat.
- `TestAntigravityQuotaPersistRoundTrip` — hanya 0% dengan reset masa depan yang
  ditulis; restart memulihkannya; bacaan sehat menghapus baris.
- `TestAntigravityQuotaPersistDropsFullyExpiredRow` / `_FailOpen`.
- `TestSelectAntigravitySkipsToRevalidate` — 10 kasus: bucket 0% masa depan yang
  sudah jatuh tempo, bucket yang masih ada kuota, reset yang sudah lewat, tanpa
  entri, tanpa access token, di dalam vs lewat cooldown 5 menit, model di luar
  family, dan dua arah keputusan Argon.
- `TestRevalidateAntigravityExhaustedStampsBeforeAwait` — penelepon bersamaan
  menghasilkan **satu** panggilan kuota; panggilan di dalam cooldown tidak
  menambah, setelah cooldown menambah.
- `TestRevalidateAntigravityExhaustedFailOpen` — refresh error tidak mengubah
  cache dan tidak menggagalkan apa pun.
- `TestAntigravityFamilyKey`, `TestAntigravityArgonBucket` — pemetaan family, dan
  aturan Argon: Argon habis memblokir walau `gemini_weekly` 100%, Argon sehat
  tidak diblokir oleh `gemini_weekly` 0%, fallback Ultra memakai family.
- `TestParseAntigravityEligibility_LoadCodeAssist`,
  `TestParseAntigravityQuotaEligibility_403`, `TestAntigravityVerdictFromErrorBody`
  — kedua parser (termasuk "tanpa field = tanpa opini") dan pengambilan link
  verifikasi lewat allowlist ban.

`go build ./...` bersih, `go vet` bersih pada `internal/handlers/chat`,
`internal/handlers/dashboard`, `internal/handlers/media`, `internal/app`, dan
`go test ./internal/handlers/chat/ ./internal/providers/ ./internal/db/` hijau
(termasuk `-race` untuk test baru).