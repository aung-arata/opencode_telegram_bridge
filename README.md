# opencode_telegram_bridge

A simple Python Telegram bot script for sending notifications and proxying messages to a local [OpenCode](https://opencode.ai) process. Uses only well-known packages (`requests`).

---

## Scripts

### `send_telegram.py` — one-shot notification sender

```
python3 send_telegram.py "Your message text here"
```

Sends a single message to the chat configured by `TG_CHAT_ID`. Useful for automation / container notifications.

---

### `polling_bot.py` — interactive bot + OpenCode proxy

Polls Telegram for messages from **you** (identified by `TG_USER_ID`) and forwards them to a local OpenCode process as if you had typed them into its terminal.

```
python3 polling_bot.py
```

**How it works:**

1. Any plain-text message you send to the bot (not starting with `/`) is written to OpenCode's stdin.
2. The bot waits for OpenCode to respond (up to `OPENCODE_RESPONSE_TIMEOUT` seconds, cutting off after `OPENCODE_IDLE_TIMEOUT` seconds of silence).
3. The response is sent back to you as a reply in Telegram.
4. Requests are serialized (queued), so concurrent messages are handled one at a time.
5. All queries and responses are logged with timestamps to `runtime/oc_bridge.log`.

**Bot commands:**

| Command | Description |
|---|---|
| `<any plain text>` | Forward as query to OpenCode |
| `/ask <query>` | Explicit OpenCode query |
| `/echo <msg>` | Bot replies with the same text |
| `/help` | Show command reference |

**Only `TG_USER_ID` can trigger queries; all other senders are silently ignored.**

---

## Configuration

Add a `.env` file to the project root:

```env
TG_BOT_TOKEN=123456:abcdeFghijKLMNOPqrs_tuvwxYZ
TG_USER_ID=123456789
TG_CHAT_ID=123456789

# Optional — OpenCode proxy tuning
OPENCODE_CMD=opencode          # command used to launch OpenCode (default: opencode)
OPENCODE_IDLE_TIMEOUT=2.0      # seconds of stdout silence = response complete (default: 2.0)
OPENCODE_RESPONSE_TIMEOUT=30   # hard cap in seconds before giving up (default: 30)
```

| Variable | Required by | Purpose |
|---|---|---|
| `TG_BOT_TOKEN` | both scripts | Telegram bot token |
| `TG_USER_ID` | `polling_bot.py` | Your Telegram numeric user ID |
| `TG_CHAT_ID` | `send_telegram.py` | Target chat for notifications |
| `OPENCODE_CMD` | `polling_bot.py` | Shell command to launch OpenCode |
| `OPENCODE_IDLE_TIMEOUT` | `polling_bot.py` | Idle cutoff for response detection |
| `OPENCODE_RESPONSE_TIMEOUT` | `polling_bot.py` | Hard deadline per query |

---

## Runtime files

All runtime state is kept in the `runtime/` directory (git-ignored, safe to delete):

| File | Purpose |
|---|---|
| `runtime/last_update_id.txt` | Last processed Telegram update (polling resume point) |
| `runtime/oc_bridge.log` | Timestamped log of all queries and responses |

---

## Error handling

- If OpenCode is not installed or the command fails, the bot replies with a descriptive error message instead of crashing.
- If the OpenCode process exits mid-conversation, the bot attempts to restart it on the next query.

---

## License
MIT

---
Co-authored-by: aung-arata <5259204+aung-arata@users.noreply.github.com>
Co-authored-by: oc-ghcp-gpt41 <oc-ghcp-gpt41@users.noreply.github.com>
