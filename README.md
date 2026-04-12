# opencode_telegram_bridge

A simple Python Telegram bot script for sending notifications (and soon, processing commands) from containers or automation scripts. Uses only well-known packages (requests).

## Usage

```
python3 send_telegram.py "Your message text here"
```

## Configuration
- Add a `.env` file to the project root with:
  - `TG_BOT_TOKEN` — your Telegram bot token
  - `TG_USER_ID`   — your personal Telegram numeric user ID (commands only accepted from this user)

Example `.env`:

```
TG_BOT_TOKEN=123456:abcdeFghijKLMNOPqrs_tuvwxYZ
TG_USER_ID=123456789
```

- Bot replies to the chat where the command was received (works for private and group chats, but only you can control it).

## License
MIT

---
Co-authored-by: aung-arata <5259204+aung-arata@users.noreply.github.com>
Co-authored-by: oc-ghcp-gpt41 <oc-ghcp-gpt41@users.noreply.github.com>
