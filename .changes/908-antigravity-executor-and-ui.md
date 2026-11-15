### fix(antigravity): identitas agen, kuota reset presisi, katalog registry, dan badge verifikasi

Port sisa lane Antigravity di luar mesin blokir ticket 907: envelope identitas
agen, pembacaan reset 409/429, baris katalog registry, split input gambar, dan
permukaan dashboard (badge "Needs verification").

**1. Envelope identitas agen (`internal/translator/antigravity.go`).**
`requestType: "agent"` + kantong `labels` kini dikirim bersama, persis capture Hub
2.32.1 di upstream (`open-sse/executors/antigravity.js`, AGENTS.md "requestType
together with labels"). Port sebelumnya membuang `requestType` sepenuhnya atas
dasar decolua/9router#3986; capture yang lebih baru menyatakan yang salah adalah
`requestType` **tanpa** labels (itu yang dijawab 429 RESOURCE_EXHAUSTED tanpa
detail), jadi keduanya harus jalan bersama. `labels` memuat `cascade_id`,
`root_cascade_id`, `trajectory_id`, `request_id`
(`<trajectory>-<last_step_index>`), `last_step_index`, dan flag
`used_claude` / `used_claude_conservative` / `used_non_gemini_model` — semuanya
string; `model_enum` sengaja tidak dikirim (tabel placeholder Hub, tidak ada
pemetaannya — upstream juga membuangnya). `sessionId` kini ikut di dalam
`request`. `requestId` yang sudah berbentuk
`agent/<conversation>/<epoch_ms>/<trajectory>/<step>` dipakai ulang (beserta id
yang dikandungnya) alih-alih menerbitkan identitas kedua untuk turn yang sama.
Batas `maxOutputTokens` dinaikkan 64000 → **65536**: Hub mengirim tepat angka itu
di jalur agen, dan 64000 adalah `maxOutput` *default* capabilities.js, bukan
angka baris pola Gemini 3/4.

**2. Reset presisi dari body 409/429 (`internal/handlers/chat/retry_after.go`).**
`retryAfterWait` kini membaca `details[].metadata.quotaResetTimeStamp` (ISO-8601,
baik di `error.details` maupun `details` telanjang — upstream toleran keduanya)
dan dua header `x-ratelimit-reset-after` (detik) / `x-ratelimit-reset` (epoch
detik) yang dibaca `AntigravityExecutor.parseRetryHeaders`. Sebelum ini body yang
hanya menyebut instant ISO tidak menghasilkan cooldown sama sekali —
`retryableCooldownSec` jatuh ke basis burst 2 detik dan akun yang habis kembali
terpilih.

**3. Katalog registry (`internal/providers/`).** `gemini-3-flash` (baris registry
upstream, di-alias ke tier 3.8-high sesuai `AntigravityModelSynonyms`) masuk
katalog Go **dan** `web/src/lib/models.ts` blok `ag` (parity katalog dashboard
dijaga `TestDashboardCatalogListsEveryRoutedModel`). Baris lanjutan lane
(`claude-sonnet-4-6` 1000000/128000, `gemini-pro-agent` 1048576/64000,
`gpt-oss-120b-medium` 128000/64000) masuk lewat `providerTokenLimits["antigravity"]`
+ alias `ag`, karena tabel substring bersama membacanya 200000/8192 dan
128000/4096. Batas pola provider-blind `*gpt-oss*` (128000/64000) dan
`gemini-pro*` (1048576/64000) juga ditambahkan agar sama dengan upstream di semua
lane. Baris 3.5-family / `gemini-3-flash-agent` tetap TIDAK diiklankan (retired,
400/404). Perubahan awal yang menaruh limit 1M/128K di `modelCapabilities` global
dibatalkan: ia menimpa deklarasi registry devin-cli (200000) untuk id yang sama —
`TestDevinCLIContextLengthsMatchRegistry` menangkapnya.

**4. Input gambar (`internal/translator/antigravity.go`).** `AntigravityImageInput`
memecah data URI (`data:image/jpeg;base64,…`) jadi mimeType + payload seperti
`resolveImageInput` upstream; sebelumnya URI dikirim utuh ke field base64
(tidak bisa didekode) dan setiap edit dilabeli `image/png`.

**5. Badge dashboard.** `usageResult.toResponse()` menyerialkan verdict
`ineligible` (bentuk upstream route.js), dan jalur fallback (koneksi tanpa
pembacaan) memakai verdict cache lewat `antigravityCachedVerdict`. Backend:
`internal/handlers/dashboard/usage_providers.go` + `usage.go` (dua hunk ini sudah
dikoordinasikan dengan pemilik ticket 907, yang mempertahankan
`RecordAntigravityEligibility` diberi verdict **segar** sehingga "tanpa opini"
tetap menghapus flag lama). Frontend: `web/src/lib/antigravityEligibility.ts`
(seleksi state murni: flagged / healthy / unknown),
`web/src/components/quota/AntigravityEligibilityBadge.svelte` (Svelte 5 runes,
token desain yang ada), dirender di `QuotaTrackerView.svelte` dari
`quota.raw.ineligible`, plus `ineligible` di `ConnectionUsageResponse`
(`web/src/api/client.ts`).

**Status per file upstream**

| upstream | status |
|---|---|
| `open-sse/executors/antigravity.js` | sudah ada sebelumnya: cloakTools (decoy + suffix `_ide` + rename riwayat + toolConfig VALIDATED), fixContents (role functionResponse, buang part thought-only, backfill thoughtSignature), sanitasi skema parameter, cap output, parseError 400 fatal (via errclass), parseRetryHeaders/parseRetryFromErrorMessage (via `retryAfterWait` + `extractResetDuration`), model gambar + aspect ratio + non-stream. **Diport sekarang:** `requestType`+`labels`+reuse `requestId`, cap 65536, `quotaResetTimeStamp`, header `x-ratelimit-reset*`. **Sengaja tidak diport:** `model_enum` (tanpa pemetaan), `sanitizeFunctionName` (ditutup di §Penutupan gap), User-Agent Hub (`antigravity/hub/2.32.1 (…cl=1021296516)`) — lihat §Catatan. |
| `open-sse/services/usage/antigravity-weekly.js` | sudah diport (`ParseWeeklyQuotaSummary`, `FetchAntigravityWeeklyQuota` di `antigravity_quota.go`: grup argon→gemini→claude/gpt, `remainingFraction`→used/1000, TTL 3 menit, dedup in-flight, bucket 5h ditambahkan ticket 907). **Ditutup di §Penutupan gap (item 1):** `resetTime` dulu hanya `time.RFC3339`, sedangkan `parseResetTime` upstream menerima epoch detik/ms (angka maupun string) — bucket dengan resetTime numerik kehilangan `ResetAt`. |
| `open-sse/providers/registry/antigravity.js` | alias `ag`, priority 20, serviceKinds llm/image/webSearch, notice RISK, oauth (client id/secret, refreshLeadMs 5m), usage URL, `searchViaChat` (default `gemini-2.5-flash`) semua sudah ada. **Diport sekarang:** id `gemini-3-flash`, jendela `gemini-pro-agent`/`claude-sonnet-4-6`/gpt-oss. **Sengaja tidak diport:** baris 3.5-family + `gemini-3-flash-agent` (retired, sudah tercatat di `AntigravityRetiredModelIDs`); `upstreamModelId` ber-suffix effort "(high)" (suffix adalah notasi thinking panrouter, 404 di wire). |
| `open-sse/handlers/imageProviders/antigravity.js` | sudah diport (`handleAntigravityImage`: default `gemini-3.1-flash-image`, suffix aspect ratio dari `size`, rotasi akun, envelope executor, normalisasi ke `{created,data}`). **Diport sekarang:** `resolveImageInput` (data URI dipecah, mime dihormati). |
| `…/ProviderLimits/AntigravityEligibilityBadge.js` | **diport sekarang** sebagai Svelte (lihat §5), termasuk sumber datanya. |

**Verifikasi**

- `go test ./internal/proxy/executor/ ./internal/providers/ ./internal/handlers/media/ ./internal/handlers/dashboard/` hijau; `go vet` bersih pada keempat paket + `internal/translator` + `internal/handlers/chat`.
- Gagal-sebelum-bukti: `TestQuotaResetTimestamp_*` (4 assertion gagal tanpa carrier ISO — diverifikasi dengan menonaktifkan satu baris `note(...)`; body berbentuk upstream, termasuk 409 dan varian detail-only), `TestRateLimitResetHeaders`, `TestWrapForAntigravity_AgentIdentity` / `_LabelFlagsFollowTheModelFamily` / `_KeepsIncomingRequestID` (tes lama yang justru menuntut `requestType` absen diganti), `TestWrapForAntigravity_CapAndRequestID` (65536), `TestAntigravityImageInput` + `TestHandleImage_Antigravity_DataURIImageIsSplitForTheWire`, `TestAntigravityCatalogMatchesRegistry` (+`_RegistryWireModel`; probe pra-perubahan: `gemini-pro-agent` 128000/4096, `claude-sonnet-4-6` 200000/8192, `gemini-3-flash` tidak terdaftar), `TestUsageResultSerialisesEligibility`, `TestHandleGetConnectionUsage_AntigravityBadgeFallback`, `TestHandleGetConnectionUsage_NoOpinionReadingClearsTheBadge`, `TestHandleGetConnectionUsage_AntigravityFreshVerdictWins`.
- Web: `bun test src/lib/antigravityEligibility.test.ts src/api/client.test.ts` (26 lulus) dan `cd web && bun run build` sukses (`tsc -b && vite build`).

**Catatan — gap yang DITEMUKAN tapi tidak ditutup**

1. **User-Agent wire.** Upstream kini mengirim identitas klien *Hub*:
   `antigravity/hub/2.32.1 (aidev_client; os_type=<host>; arch=<host>; cl=1021296516)`
   (`open-sse/providers/shared.js:116-133`, dijaga
   `tests/unit/antigravity-client-identity.test.js`). Port ini mengirim
   `antigravity/cli/1.5.1 (aidev_client; os_type=windows; arch=amd64; cl=1014269460; auth_method=consumer)`
   (`internal/providers/client_identity.go:47-50`) dan punya setelan profil
   operator ide|cli (`antigravity_profile.go`). Ini keputusan live-wire yang
   menyentuh setiap call site antigravity (chat, media, kuota, probe dashboard):
   capture port (2026-11-05) menyatakan flavor **cli** yang dijawab lane Argon
   sementara flavor `ide/<ver>` 404, dan upstream sendiri mengakui perubahannya
   belum membuktikan Argon resolve. Karena itu saya laporkan, tidak membalik
   fingerprint produksi.
2. **`sanitizeFunctionName`.** *Ditutup di §Penutupan gap (item 3).* Upstream menyanitasi nama tool ke
   `[a-zA-Z_][a-zA-Z0-9_.:\-]{0,63}` saat menggabungkan deklarasi; port belum
   punya padanannya, jadi nama tool tak valid dari klien (spasi, awalan digit)
   masih bisa ditolak Gemini. Belum saya port karena pemetaan decloak
   (suffixed→asli) perlu ikut menyimpan nama pascasanitasi agar nama panggilan
   balik tetap bisa dipulihkan — perubahan kecil tapi menyentuh jalur uncloak
   yang dipakai `internal/handlers/chat`; tidak dikerjakan di bawah budget ini.
3. **Mirror verdict ke `lastError`.** Upstream juga menulis penanda
   "Account needs verification (VALIDATION_REQUIRED) …" ke `providerConnections.lastError`
   supaya halaman Providers ikut menandai akun (route.js:225-244). Port ini tidak
   punya penulis "lastError saja": `RecordConnectionScopedError` ikut menulis
   `status`/`backoffLevel`/`rateLimitedUntil`, jadi memakainya akan mengubah
   status kesehatan koneksi dan backoff — efek samping yang tidak dimiliki update
   bertarget upstream. Perlu API repo baru sebelum diport.
4. **Pola error transien `isTransientAntigravityError`** ("high traffic",
   "agent execution terminated due to error", "stream ended/closed/...", "empty
   response") — *ditutup di §Penutupan gap (item 6).* Tidak diport sebagai satu daftar: jalur retry port memakai
   `errclass` + `retryAfterWait` + watchdog stream, bukan daftar regex provider.
   Perilaku teramati (429/5xx transien diulang, body tanpa instruksi tidak
   memperpanjang) tertutup lewat tes yang ada; tidak ada tes baru yang bisa
   "gagal sebelum" tanpa memindahkan seluruh kebijakan retry.
5. **`sessionId` numerik** (`toNumericSessionId`) — *diverifikasi: sudah tertangani, lihat §Penutupan gap (item 4).* Tidak diport: `sessionId` port
   berasal dari header sesi klien/koneksi, dan konversi numerik hanya relevan
   untuk Hub yang mengirim id numeriknya sendiri.
6. **`maxOutputTokens` claude** pada baris antigravity (`claude-opus-4-6-thinking`
   8192 di port vs 64000 default upstream) — *ditutup di §Penutupan gap (item 5).* Sebelumnya tetap seperti semula: itu lantai
   provider-blind untuk semua lane claude, bukan keputusan lane antigravity, dan
   mengubahnya di situ akan menerbitkan ulang limit `/v1/models` semua jalur
   claude. Baris 3.1-pro-low juga tetap 65536 (upstream menulis 65535 di
   `*gemini-3*pro*`).

**Penutupan gap (ticket lanjutan)**

Enam item yang dilaporkan di atas, ditutup/diverifikasi satu per satu. Semua tes
baru memakai `testing.T`, table-driven, tanpa tidur; jaringan hanya lewat server
`httptest` lokal; waktu dan kuota lewat seam yang sudah ada (`agNow`,
`antigravityQuotaBaseURL`).

| # | item | status | bukti |
|---|---|---|---|
| 1 | `parseResetTime` (epoch detik/ms) | **ditutup** | `ParseAntigravityResetTime` (`internal/handlers/chat/antigravity_quota.go`) mengikuti urutan upstream (`open-sse/services/usage/shared.js:15-42`): angka (JSON number maupun string all-digit) → detik bila `< 1e12`, ms bila tidak; selain itu RFC3339; sisanya tanpa reset (zero time). Medan `resetTime` jadi `any` di bucket family **dan** baris per-model — tipe `string` dulu membuat payload numerik gagal decode seluruhnya. Tes: `TestParseWeeklyQuotaSummary_ResetInstantShapes` (RFC3339, epoch detik/ms sebagai number dan string, garbage, null) + `TestRefreshAntigravityQuota_EpochResetTime`. Gagal-sebelum: 5 assertion (4 bentuk epoch + baris per-model) kembali zero saat cabang angka dinonaktifkan. Deviasi kecil: epoch `0`/negatif → zero time (bukan 1970), karena tidak ada konsumen yang membaca instan lampau sebagai reset. |
| 2 | instant reset di body 409/429 | **ditutup** | `quotaResetInstant` (`internal/handlers/chat/retry_after.go`) menyusun carrier yang sudah ada — `quotaResetTimestamp` (ISO `quotaResetTimeStamp`), `retryInfoDelay`, `quotaResetDelay` — tanpa clamp 2 jam, lalu `HandleAntigravityQuotaError` (`antigravity_quota.go:461`) mem-park model di instan itu **sebelum** membaca kuota live, persis urutan upstream (`open-sse/executors/antigravity.js:455-503` parseError; `src/sse/handlers/chat.js:443` hanya memanggil `handleAntigravityQuotaError` saat `!resetsAtMs`). Tes: `TestHandleAntigravityQuotaError_ParksOnTheBodyResetInstant` (409 dan 429; instan body 75m, baris live 2 jam → harus 75m, 0 panggilan kuota, cache ter-block), `_ParksOnTheDelayTheBodyNames` (delay `74h19m34s` → park 74 jam, bukan clamp 2 jam) + `_NonQuotaStatusHasNoPark`. Gagal-sebelum: dengan short-circuit dimatikan, tes pertama melaporkan instan 2 jam dan 1 panggilan kuota, tes delay melaporkan `no park deadline`. `quotaResetTimestamp`/`parseRateLimitResetHeaders` dan tes lamanya tetap hijau (hanya memakai decoder detail bersama `rateLimitDetails`). Prosa "Resets in …" tanpa detail terstruktur tetap lewat `extractResetDuration` (clamp 2 jam): jalur park memakai carrier yang menyebut instan/delay terstruktur, bukan prosa. |
| 3 | `sanitizeFunctionName` | **ditutup** | `SanitizeFunctionName` (`internal/translator/antigravity.go`, port `open-sse/executors/antigravity.js:12-19`) dijalankan pada setiap deklarasi (native, klien, decoy) dan pada riwayat `functionCall`/`functionResponse`; nama wire = `sanitize(name + "_ide")` dan **map decloak dikunci pada nama pascasanitasi** → nama asli klien. Tes: `TestCloakAntigravityRequest_SanitizesFunctionNames` (a: spasi, awalan digit, slash, truncate 64 byte; semua deklarasi cocok `^[a-zA-Z_][a-zA-Z0-9_.:\-]{0,63}$`), `_SanitizesHistoryToolCalls`, `TestSanitizeFunctionName` (b: `UncloakToolName(wireName, map)` mengembalikan ejaan klien). Gagal-sebelum: 8 assertion saat sanitasi dilepas (wire `"my tool_ide"`, map tak cocok). |
| 4 | `sessionId` numerik | **sudah tertangani** | `sessionId` port selalu `string` di semua titik: `handlerutil.ExtractSessionID` hanya membaca header (`internal/handlerutil/response.go`, `SessionHeaderKeys`), `hardenAntigravityRequest` menulis `m["sessionId"] = sessionID` (`internal/translator/antigravity.go`), dan `AntigravityRequest.Request` adalah `jsontext.Value` mentah sehingga tidak ada medan bertipe yang bisa gagal decode. Nilai numerik dari body klien ditimpa (bukan error), jadi envelope tidak pernah rusak. Tes karakterisasi: `TestWrapForAntigravity_SessionIDShapes` (numeric/string/null/absent → `request.sessionId` selalu string non-kosong, envelope valid). Tidak ada perubahan perilaku; `toNumericSessionId` tetap tidak diport (id port berasal dari header sesi, bukan dari Hub). |
| 5 | lantai `maxOutput` claude | **ditutup** | `antigravityClaudeMaxOutputFloor = 64000` (`internal/providers/capabilities.go`), sumbernya `DEFAULT_CAPABILITIES.maxOutput` (`open-sse/providers/capabilities.js:63`) — angka yang upstream resolve untuk baris claude di lane ini, karena pola `*claude*opus*`/`*claude*sonnet*` hanya menyatakan flag + bentuk thinking. Berlaku hanya untuk provider `antigravity`/`ag` (alias di-resolve) dan hanya bila hasil resolusi di bawah lantai; deklarasi kustom operator menang. Tes: `TestAntigravityClaudeMaxOutputFloor` (lane + alias 64000, baris deklarasi 128000 tetap, lane lain 8192 tetap, gemini 65536 tetap), `_RespectsCustomRows`, dan `TestAntigravityCatalogMatchesRegistry` (baris `claude-opus-4-6-thinking` kini 200000/64000, deviasinya dicabut). Gagal-sebelum: 4 assertion pada 8192 saat konstanta dinolkan. |
| 6 | pola error transien | **ditutup** | Sepuluh baris `Provider: "antigravity"` di `ErrorRules` (`internal/providers/errorclassify.go`) memetakan daftar upstream (`open-sse/executors/antigravity.js:26-42`): high traffic, agent (execution) terminated due to error, temporarily unavailable, timeout, stream ended/closed/terminated/interrupted, empty response ("capacity" sudah ada). Provider-scoped supaya prosa yang sama di lane lain tetap request-scoped. Tes: `TestClassifyErrorForProvider_AntigravityTransientBodies` (10 bodi @400 → fallback level 1), `_TransientBodiesStayAntigravityScoped` (openai/codebuddy-intl/gemini/"" tetap request-scoped), `_AntigravityTransientKeepsRateLimitSemantics`. Gagal-sebelum: 2 assertion `ShouldFallback:false` saat baris dipindah ke provider fiktif. **Batas 429 CodeBuddy utuh:** `stopsRotationOnRateLimit` (`internal/handlers/chat/fallback.go:1677`) hanya membaca status + provider, tidak pernah `ErrorRules`; baris `{Status: 429, Backoff: true}` tidak disentuh dan aturan baru semuanya text-based + provider-scoped, jadi tidak ada jalur yang membuat 429 CodeBuddy berjalan ke akun berikutnya. Diverifikasi dengan `go test ./internal/handlers/chat/ -run 'TestHandleAccountFallback_CodeBuddyRateLimitStopsRotation|TestStopsRotationOnRateLimit'` dan seluruh paket hijau. Catatan: lane media (`antigravity_image.go`, `antigravity_search.go`, `antigravity_stt.go`) memanggil `providers.ClassifyError` tanpa provider — klasifikasi media tetap provider-agnostik seperti sebelumnya, jadi daftar ini belum berlaku di sana. |
| — | User-Agent cli vs Hub | **sengaja tidak diubah** | keputusan live-wire dengan capture (lihat §Catatan 1); di luar ticket ini. |

**Verifikasi.** `go test ./internal/handlers/chat/ ./internal/translator/ ./internal/providers/ ./internal/proxy/executor/` hijau; `go vet` bersih pada keempat paket + `./internal/proxy/...`; tes `retry_after.go` lama (`antigravity_reset_timestamp_test.go`, `rate_limit_failover_test.go`, `retry_after_body_test.go`) tetap hijau setelah `quotaResetTimestamp` memakai decoder bersama `rateLimitDetails`.
