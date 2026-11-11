# muse-router

AI router di atas **Muse Code subscription**: satu kredensial Muse diekspos
kembali sebagai endpoint yang kompatibel dengan OpenAI dan Anthropic.

- `POST /v1/chat/completions` — OpenAI chat (teks, image, tool calls, stream SSE)
- `POST /v1/responses` — OpenAI Responses (passthrough + quirk-fix)
- `GET /v1/responses/{id}` — ambil stored run (nama tool di-decode bila run dibuat router ini)
- `POST /v1/messages` — Anthropic Messages (termasuk tool_use/tool_result, stream)
- `GET /v1/models` — katalog model (statis + discovery upstream, non-chat difilter)
- `GET /healthz` — health check tanpa auth

Auth ganda: **bearer token router** untuk klien (`ROUTER_API_KEYS`), dan
**kredensial Muse** (`oauthAccessToken` + subscription `apiKey`) untuk upstream
`https://api.meta.ai/v1`. Referensi auth Muse: `packages/ai/src/registry/oauth/muse-code.ts`
dan `packages/catalog/src/compat/rules/{auth,providers}/muse-code.kdl` di repo ini.

Tech stack: **Go 1.24, stdlib `net/http` saja** (tanpa framework), clean architecture.

## Arsitektur

```text
cmd/muse-router          CLI: serve | login | models | version
internal/
  domain/                Tipe wire (chat/responses/messages), kredensial,
                         error bertipe. Tanpa dependensi.
  translator/            chat<->responses, messages<->responses, sanitasi
                         responses. Di sini quirks Muse diterapkan:
                         prefix tool "_", drop tool_choice, validasi effort.
  usecase/               Satu turn: resolve model -> translate -> upstream ->
                         translate balik. Port MuseClient/CredentialProvider/
                         ModelCatalog didefinisikan di sini.
  infrastructure/
    config/              ENV -> Config.
    muse/                HTTP client api.meta.ai, SSE reader, key-mint,
                         device-code login, katalog statis.
    auth/                CredentialStore (JSON/file/mint/API key) +
                         validasi bearer router.
  delivery/http/         Route, middleware (auth/CORS/log/recovery),
                         envelope error per protokol, framing SSE + keepalive.
```

Alur request: `delivery -> usecase -> translator -> muse client -> api.meta.ai`,
lalu diterjemahkan balik ke format asal. `/v1/responses` adalah passthrough
(memperbaiki nama tool + menghapus `tool_choice`); dua endpoint lain
diterjemahkan penuh termasuk tool calls dan stream.

## Mulai cepat

```bash
cd muse-router
go build -o muse-router ./cmd/muse-router

# 1. Login sekali (device-code di browser), kredensial tersimpan 0600 di
#    ~/.muse-router/credential.json
./muse-router login

# 2. Jalankan router
export ROUTER_API_KEYS=sk-router-ganti-saya
./muse-router serve
# listening on http://127.0.0.1:4000
```

Tanpa login interaktif, isi salah satu: `MUSE_CREDENTIAL_JSON` (JSON hasil login),
`MUSE_OAUTH_TOKEN` (auto-mint sekali lalu di-cache), atau `MUSE_API_KEY`
(Model API key langsung). Lihat `.env.example`.

## Contoh

```bash
BASE=http://127.0.0.1:4000 KEY=sk-router-ganti-saya

# Models
curl -H "Authorization: Bearer $KEY" $BASE/v1/models

# Chat non-stream dengan tools
curl -H "Authorization: Bearer $KEY" $BASE/v1/chat/completions -d '{
  "model": "muse-spark-1.3",
  "messages": [{"role": "user", "content": "list files"}],
  "tools": [{"type": "function", "function": {
    "name": "bash", "description": "Run.",
    "parameters": {"type": "object", "properties": {"command": {"type": "string"}}}
  }}]
}'

# Chat stream (akhiri dengan data: [DONE])
curl -N -H "Authorization: Bearer $KEY" $BASE/v1/chat/completions -d '{
  "model": "muse-code/muse-spark-1.3", "stream": true,
  "messages": [{"role": "user", "content": "hai"}]
}'

# Responses passthrough
curl -H "Authorization: Bearer $KEY" $BASE/v1/responses -d '{
  "model": "muse-spark-1.3", "input": "hai",
  "reasoning": {"effort": "low"}
}'

# Anthropic Messages dengan tool_use
curl -H "Authorization: Bearer $KEY" $BASE/v1/messages -d '{
  "model": "muse-spark-1.3", "max_tokens": 1024,
  "messages": [{"role": "user", "content": "list files"}],
  "tools": [{"name": "bash", "description": "Run.",
    "input_schema": {"type": "object"}}]
}'
```

ID model boleh telanjang (`muse-spark-1.3`) atau terkualifikasi
(`muse-code/muse-spark-1.3`); upstream selalu menerima ID telanjang.

## Quirks Muse yang ditangani

- **Nama tool di-prefix `_`** saat dikirim (`bash` -> `_bash`), kecuali nama
  64 karakter (batas upstream) yang dikirim apa adanya; tabrakan dua nama ke
  satu wire name ditolak 400. Nama dikembalikan ke aslinya di respons dan
  stream. Wire name tidak pernah bocor ke klien.
- **`tool_choice` selalu dibuang** — upstream hanya menerima `auto`.
  Pengecualian: Anthropic `tool_choice: {"type": "none"}` dipenuhi dengan
  tidak mengirim tools sama sekali.
- **`store`** default dari `MUSE_STORE_RESPONSES` (default off, demi privasi);
  klien boleh override per request di chat/responses.
- **Reasoning effort** `minimal/low/medium/high/xhigh/max` divalidasi lalu
  di-clamp ke ladder model yang di-resolve (`max` → `xhigh` di model 5-tier);
  model tak dikenal passthrough (upstream yang menilai).
  Anthropic `thinking: disabled` dipetakan ke `minimal`.
- **`/v1/responses` meneruskan field tak dikenal** (`background`, `truncation`,
  `text`, `parallel_tool_calls`, ...) apa adanya ke upstream; hanya `model`,
  `input`, `tools`, `reasoning.effort`, `store`, `stream` yang disentuh dan
  `tool_choice` yang dibuang.
- **`/v1/models` memfilter id non-chat** hasil discovery (`sam-*`,
  `*transcribe*`, `*image*`); famili tak dikenal tetap di-merge optimistis.
- **Key endpoint rate-limited**: mint hanya sekali per proses lalu di-cache;
  kredensial JSON/file tidak pernah re-mint.

## Konfigurasi

| ENV | Default | Keterangan |
|---|---|---|
| `ROUTER_BIND` | `127.0.0.1:4000` | Alamat listen. |
| `ROUTER_API_KEYS` / `ROUTER_API_KEY` | kosong (tanpa auth) | Bearer token klien, koma-pisah. |
| `MUSE_CREDENTIAL_JSON` | — | JSON `{oauthAccessToken, apiKey}` dari `login`. |
| `MUSE_CREDENTIAL_FILE` | `~/.muse-router/credential.json` | File kredensial (baca + tulis login). |
| `MUSE_OAUTH_TOKEN` | — | Token akun untuk auto-mint sekali. |
| `MUSE_API_KEY` | — | Model API key langsung (tanpa mint). |
| `MUSE_BASE_URL` | `https://api.meta.ai/v1` | Upstream inference. |
| `MUSE_KEY_URL` | `https://api.meta.ai/muse-code/key` | Key-mint + entitlement. |
| `MUSE_STORE_RESPONSES` | `0` | Default `store` upstream. |
| `MUSE_TIMEOUT_SECONDS` | `600` | Timeout header/first-byte upstream. |

Urutan kredensial: JSON > file > API key langsung > mint dari OAuth token.

## Development

```bash
go vet ./...     # harus bersih
go test ./...    # unit + e2e mock upstream
go build ./...
```

Test e2e (`internal/delivery/http/e2e_test.go`) menjalankan stack asli
(server + usecase + translator + Muse client) melawan mock `api.meta.ai`
dan menegaskan: prefix tool di upstream, decode di respons, framing SSE
(`[DONE]` untuk chat/responses, `event:` untuk messages), merge `/v1/models`,
dan 401 tanpa bearer.

## Keterbatasan (v1)

- Hanya model teks+image Muse; endpoint image/audio/embeddings tidak ada.
- Opsi tanpa padanan Responses di-drop diam-diam: `stop`, presence/frequency
  penalty, `top_k`, Anthropic `service_tier`/`metadata`, budget `thinking`.
- Hosted/upstream-only tools (web search dsb.) tidak diterjemahkan ke
  chat/messages; di `/v1/responses` diteruskan apa adanya.
- `previous_response_id` diteruskan apa adanya (butuh `store: true`).
- `GET /v1/responses/{id}` hanya mendecode nama tool untuk run yang dibuat
  oleh instance router ini (cache wiremap 1 jam, maks 10 ribu); id lain
  dikembalikan mentah apa adanya.
- Mid-stream upstream failure menutup stream (terpotong); kegagalan sebelum
  chunk pertama dikembalikan sebagai error JSON dengan status asli.
