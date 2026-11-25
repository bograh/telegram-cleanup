# Telegram Cleanup

A local dashboard for decluttering a bloated Telegram account. It connects to
**your** Telegram account, lists every channel, group and bot you're in, asks an
AI model to categorize them and suggest what to keep, mute, archive or leave,
and lets you act on those suggestions in bulk.

Built with **Go + HTMX + templ**. It talks to Telegram through MTProto
([gotd/td](https://github.com/gotd/td)), so it sees your real chat list. The Bot
API can't do that. The AI works with any **OpenAI-compatible** Chat Completions
API: OpenAI, OpenRouter, Groq, Together, Ollama, LM Studio, vLLM and others.

## What you get

- **Overview**: counts of channels, groups, bots, chats with deleted accounts and unread messages, plus an AI-written summary of what your Telegram is mostly made of and a few quick wins.
- **Per-chat verdicts**: a category (news, crypto & trading, promo & spam, tech, community, …), a one-line summary, a noise score from 0 to 10, and a suggestion (keep, mute, archive or leave) with a short reason.
- **Activity stats**: messages per day (estimated from recent posts), members, unread count and time since the last post.
- **Filters and sorting**: by type, category or suggestion, or by noisiest, most unread or least recent.
- **Bulk actions**: tick rows or use quick-select (all shown, channels, groups, bots, deleted accounts, suggested leave/archive/mute, has unread), then Leave, Mute, Archive or Mark read. Quick-select works together with the filters, so "Channels" + the `crypto & trading` filter selects exactly those. Bulk runs happen in the background with progress and a **Stop** button. They're spaced out to avoid Telegram rate limits, and they skip chats that are already muted, archived or read. Leaving bots blocks them and deletes the chat; leaving a deleted account's chat just deletes it.
- **Owner protection**: bulk Leave never touches channels or groups you created. Leave those one at a time from the row's button if you really mean it.

Personal 1:1 chats with people are skipped and never sent to the AI. The exception is chats with **deleted accounts**: they're listed so you can clear them out, but their messages are never read, stored or sent to the AI.

## Setup

1. **Telegram API credentials:** go to <https://my.telegram.org/apps>, log in, and
   create an app (any name and platform). Copy the `api_id` and `api_hash`.
2. **Configure:**
   ```sh
   cp .env.example .env
   # fill in TELEGRAM_APP_ID, TELEGRAM_APP_HASH, and optionally AI_* settings
   ```
3. **Run:**
   ```sh
   make run        # or: go run .
   ```
   Open <http://127.0.0.1:8080>, enter your phone number and the code Telegram
   sends you (and your 2FA password if you have one).
4. Click **Scan Telegram**, then **Analyze with AI**.

Requires Go 1.27+. You only need templ (installed as a Go tool, `go tool templ`)
if you edit `.templ` files. The generated `*_templ.go` files are committed.

## Privacy & safety

- Everything runs on your machine. The server binds to `127.0.0.1` by default.
- `data/session.json` is a **logged-in Telegram session**. Anyone with that file
  has full access to your account. It's git-ignored; keep it private. Delete
  it to log out (and end the session under Telegram → Settings → Devices).
- For AI analysis, chat titles, usernames, stats and up to 6 short snippets of
  recent messages per channel, group or bot are sent to the AI endpoint you
  configure. If you'd rather nothing leaves your machine, use a local model
  through Ollama or LM Studio.
- Leaving is real: the app leaves the channel or group, or blocks the bot and
  deletes the chat. Rejoining a private group requires a new invite.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `TELEGRAM_APP_ID` / `TELEGRAM_APP_HASH` | — | Required |
| `AI_BASE_URL` | `https://api.openai.com/v1` | Any OpenAI-compatible base URL |
| `AI_API_KEY` | — | Sent as `Authorization: Bearer …`; may be empty for local servers |
| `AI_MODEL` | — | AI features are disabled until this is set |
| `AI_JSON_MODE` | `true` | Sends `response_format: json_object`; set `false` if your server rejects it |
| `AI_BATCH_SIZE` / `AI_CONCURRENCY` | `25` / `3` | Lower these for small local models or rate-limited free tiers |
| `AI_MAX_TOKENS` | `0` (auto) | Cap on `max_tokens`. The app also learns the provider's limit from "request too large" errors and splits batches to fit |
| `AI_REASONING_EFFORT` | — | Sent as `reasoning_effort`, e.g. `none` for Qwen3 on Groq. Thinking tokens count against output limits |
| `ADDR` | `127.0.0.1:8080` | Listen address |
| `DATA_DIR` | `data` | Session + scan cache |

## Project layout

```
main.go                       config + wiring
internal/telegram/            MTProto login, dialog scan, leave/mute/archive/read
internal/ai/                  OpenAI-compatible classifier + overview
internal/store/               JSON persistence (data/state.json)
internal/views/*.templ        templ components (pages + HTMX fragments)
internal/web/                 HTTP handlers, background jobs, static assets (htmx vendored)
```
