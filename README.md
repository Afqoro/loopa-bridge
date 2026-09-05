# loopa-bridge

OpenAI-compatible bridge for **loopa.im** (EaseClaw/EaseUS family). Connects to loopa.im's WebSocket chat API and exposes an OpenAI-compatible HTTP endpoint — supports tool calls, streaming SSE, and auto session creation.

## Features

| Feature | Status |
|---|---|
| OpenAI `/v1/chat/completions` (stream + non-stream) | ✅ |
| `/v1/models`, `/healthz` | ✅ |
| Tool calls (exec, read_file, write_file) | ✅ |
| Auto-create session (no REST, no sign) | ✅ |
| SSE streaming with tool output markers | ✅ |
| Attachment metadata in response | ✅ |
| Auth via JWT Bearer token | ✅ |
| File-based token fallback | ✅ |

## How it works

```
Client (curl/OpenAI SDK) ──HTTP──► loopa-bridge (:18768)
                                      │
                                      └──WS──► wss://www.loopa.im/nanobot-proxy-socket/api/easeclaw/ws/web:new?token=Bearer%20<JWT>
                                                │
                                                └──► Loopa backend (MiniMax M3, DeepSeek, Kimi, Qwen...)
```

Loopa.im uses WebSocket for chat + tool calls. The bridge:
1. Generates a session key client-side (`web:session-<ts>-<rand6>`)
2. Connects to loopa WS with JWT Bearer token
3. Sends chat message, receives streaming responses
4. Maps loopa WS messages → OpenAI SSE chunks / non-stream JSON
5. Tool calls (exec/read_file/write_file) are streamed inline as content

## Install

```bash
go install github.com/Afqoro/loopa-bridge@latest
```

Or build from source:
```bash
git clone https://github.com/Afqoro/loopa-bridge
cd loopa-bridge
go build -o loopa-bridge .
```

## Usage

### 1. Get JWT token

Login to loopa.im via browser, extract `auth_info` cookie, decode `accessToken` JWT. Or save to `/tmp/loopa-auth.json`:
```json
{"access_token": "eyJ0eX..."}
```

### 2. Run bridge

```bash
# Option A: env var
LOOPA_ACCESS_TOKEN=eyJ0eX... ./loopa-bridge

# Option B: auth file (default: /tmp/loopa-auth.json)
./loopa-bridge

# Custom port/auth
LOOPA_PORT=8080 LOOPA_API_KEY=secret LOOPA_ACCESS_TOKEN=eyJ0eX... ./loopa-bridge
```

### 3. Call OpenAI API

```bash
# Non-stream
curl http://127.0.0.1:18768/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "minimax/minimax-m3",
    "messages": [{"role":"user","content":"Halo, 3+3=?"}],
    "stream": false
  }'

# Stream
curl -N http://127.0.0.1:18768/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "minimax/minimax-m3",
    "messages": [{"role":"user","content":"Buat file teks Halo Dunia"}],
    "stream": true
  }'
```

### Use with OpenAI Python SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:18768/v1",
    api_key="any"  # no auth by default
)

resp = client.chat.completions.create(
    model="minimax/minimax-m3",
    messages=[{"role":"user","content":"Buat file Excel Halo Dunia"}],
    stream=True
)
for chunk in resp:
    print(chunk.choices[0].delta.content or '', end='', flush=True)
```

## Config

| Env Var | Default | Description |
|---|---|---|
| `LOOPA_PORT` | `18768` | HTTP listen port |
| `LOOPA_HOST` | `0.0.0.0` | HTTP listen host |
| `LOOPA_ACCESS_TOKEN` | (env) | JWT Bearer token for loopa.im |
| `LOOPA_AUTH_FILE` | `/tmp/loopa-auth.json` | Fallback: load token from JSON file |
| `LOOPA_WS_ENDPOINT` | `wss://www.loopa.im/nanobot-proxy-socket/api/easeclaw/ws/web:new` | WS endpoint |
| `LOOPA_DEFAULT_MODEL` | `minimax/minimax-m3` | Default model ID |
| `LOOPA_IMAGE_MODEL` | `GPT Image 2` | Image model |
| `LOOPA_VIDEO_MODEL` | `Seedance 2.0` | Video model |
| `LOOPA_AUDIO_MODEL` | `Suno` | Audio model |
| `LOOPA_TIMEZONE` | `Asia/Shanghai` | Timezone sent to loopa |
| `LOOPA_API_KEY` | (empty) | If set, requires `Authorization: Bearer <key>` on requests |

## Available models

| Model ID | Classification |
|---|---|
| `minimax/minimax-m3` | lite |
| `deepseek/deepseek-v4-flash-0731` | lite |
| `moonshotai/kimi-k2.5` | lite |
| `qwen/qwen3.6-flash` | lite |

## Limitations

- **No file download**: attachments only return metadata (file_id, name, size). Download requires REST API with sign algorithm (not implemented).
- **Single-turn session per request**: each `/v1/chat/completions` creates a new WS connection + session. Multi-turn context is flattened into the prompt.
- **No credit check**: bridge doesn't query `/api/auth/permission`. Free plan = 600 credits/day.
- **Token expiry**: JWT expires ~12h. Use refresh token endpoint to renew (not implemented — re-login via browser when expired).

## License

MIT
