# opencode_telegram_bridge

A Go daemon that bridges Telegram with a local [OpenCode](https://opencode.ai) server. Polls Telegram for messages, forwards them to OpenCode via its HTTP+SSE API, and streams responses back as edited Telegram messages (typing effect).

---

## Architecture

```
cmd/
  bridge/main.go       — entry point, wires everything together
  notify/main.go       — one-shot CLI tool to send a Telegram message
internal/
  config/config.go     — .env loader, config struct
  opencode/client.go   — HTTP client: CreateSession, SendMessage, StreamResponse (SSE)
  telegram/bot.go      — polling loop, command dispatch, send/reply helpers
  logger/logger.go     — UTC timestamped file + stdout logger
```

---

## Prerequisites

- **Go 1.21+** to build from source
- **OpenCode** running as a local HTTP server:
  ```
  opencode serve
  ```
  By default OpenCode listens on `http://127.0.0.1:4096`.

---

## Build

```bash
go build -o bridge ./cmd/bridge/
go build -o notify ./cmd/notify/
```

---

## Usage

### `bridge` — interactive bot + OpenCode proxy

```bash
./bridge
```

Polls Telegram for messages from **you** (identified by `TG_USER_ID`), forwards them to OpenCode via its HTTP API, and streams the response back.

**How it works:**

1. Any plain-text message you send to the bot (not starting with `/`) is forwarded to OpenCode as a query.
2. The bot creates an OpenCode session per Telegram chat (persistent conversation thread).
3. Responses are streamed back in real time by editing the placeholder message (typing effect).
4. Queries are serialized so concurrent messages are handled one at a time.
5. All activity is logged with UTC timestamps to `runtime/oc_bridge.log`.

### `notify` — one-shot message sender

```bash
./notify "Your message text here"
```

Sends a single message to the chat configured by `TG_CHAT_ID`. Useful for automation / container notifications.

---

## Bot commands

| Command | Description |
|---|---|
| `<any plain text>` | Forward as query to OpenCode |
| `/ask <query>` | Explicit OpenCode query |
| `/echo <msg>` | Bot replies with the same text |
| `/help` | Show command reference and status |

**Only `TG_USER_ID` can trigger queries; all other senders are silently ignored.**

---

## Configuration

Add a `.env` file to the project root:

```env
TG_BOT_TOKEN=123456:abcdeFghijKLMNOPqrs_tuvwxYZ
TG_USER_ID=123456789
TG_CHAT_ID=123456789

# OpenCode server URL (default: http://127.0.0.1:4096)
OPENCODE_URL=http://127.0.0.1:4096

# Session timeout for HTTP requests (default: 30s)
OPENCODE_SESSION_TIMEOUT=30s
```

| Variable | Required by | Purpose |
|---|---|---|
| `TG_BOT_TOKEN` | both binaries | Telegram bot token |
| `TG_USER_ID` | `bridge` | Your Telegram numeric user ID |
| `TG_CHAT_ID` | `notify` | Target chat for notifications |
| `OPENCODE_URL` | `bridge` | OpenCode HTTP server base URL |
| `OPENCODE_SESSION_TIMEOUT` | `bridge` | Timeout for OpenCode HTTP requests |

---

## OpenCode HTTP API

The bridge communicates with OpenCode via its HTTP server:

1. **`POST /session`** — create a new session, returns `{ "id": "..." }`
2. **`POST /session/{sessionId}/message`** — send a message/prompt to a session
3. **`GET /session/{sessionId}/events`** — SSE stream of response chunks

Start OpenCode in server mode before running the bridge:

```bash
opencode serve
# Default: http://127.0.0.1:4096
# Verify: curl http://127.0.0.1:4096/openapi.json
```

---

## Runtime files

All runtime state is kept in the `runtime/` directory (git-ignored, safe to delete):

| File | Purpose |
|---|---|
| `runtime/last_update_id.txt` | Last processed Telegram update (polling resume point) |
| `runtime/oc_bridge.log` | Timestamped log of all queries and responses |

---

## Graceful shutdown

The bridge handles `SIGINT` and `SIGTERM` for clean shutdown — stop polling, drain in-flight requests, and exit.

---

## License

MIT

---
Co-authored-by: aung-arata <5259204+aung-arata@users.noreply.github.com>
