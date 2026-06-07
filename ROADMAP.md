# Roadmap

Features ordered by impact. Items marked ✅ are shipped.

---

## High Impact

1. ✅ **Permission handling** — inline Yes/No buttons when OpenCode requests tool access
2. ✅ **`/abort` command** — cancel running query mid-stream via `POST /session/:id/abort`
3. ✅ **`/plan` and `/build` mode switching** — switch between read-only (plan) and full-access (build) agents
4. ✅ **`/diff` command** — `GET /session/:id/diff` shows files changed in current session; review what OpenCode touched without leaving Telegram
5. ✅ **Session persistence** — `runtime/sessions.json` survives restarts; previous session resumes automatically
6. ✅ **Session naming** — `PATCH /session/:id` with `{ title }` auto-set from first message; makes sessions recognizable in OpenCode TUI

---

## Medium Impact

7. ✅ **`/undo` command** — `POST /session/:id/revert` rolls back last message and its file changes
8. **`/sessions` command** — `GET /session` lists sessions with titles; resume an old one by ID
9. **`/history` command** — `GET /session/:id/message` shows conversation so far as a summary
10. **Telegram 429 backoff** — current 1 edit/sec is hardcoded; add exponential backoff on rate-limit errors

---

## Lower Priority / Situational

11. **Model switching (`/model`)** — `GET /provider` + `PATCH /config` to swap models from Telegram
12. **File reading (`/read <path>`)** — `GET /file/content` surfaces file contents inline
13. **Codebase search (`/find <query>`)** — `GET /find` exposes OpenCode's code search
14. **Multi-user support** — currently hardcoded to single `TG_USER_ID`; per-user sessions
15. **Health/metrics endpoint** — expose uptime, active session, query count for observability

---

## Notes

- OpenCode API reference: `curl http://127.0.0.1:4096/openapi.json`
- Items 4–8 all have direct OpenCode API endpoints; low implementation lift
- Items 10–12 require understanding OpenCode's provider/config API shape first
