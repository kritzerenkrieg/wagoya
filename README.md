# wagoya

**WhatsApp Go Assistant.** A WhatsApp number backed by an LLM, written in Go. DM it, or add it to group chats and @mention it. It has a persona of its own, a long-term memory of the people it talks to, and it can search the web.

It runs as a single static binary (no CGO, pure-Go SQLite) and links to WhatsApp as a companion device through [whatsmeow](https://github.com/tulir/whatsmeow).

## Highlights

### Knowledge system: global people memory

wagoya remembers people, not just conversations.

- **Profiles keyed by WhatsApp identity.** Every person the bot hears gets a profile keyed by their LID (or phone number), never by display name, so nobody can impersonate someone by renaming themselves.
- **Global across chats.** What it learns about someone in one group is available in every other chat and DM.
- **The model writes its own memory, through checked tools.** `save_fact`, `invalidate_fact` and `save_memory` validate every call and return errors to the model instead of failing silently. The bot, not the model, decides who said a fact and how much to trust it. Facts can be about anyone in the chat, or anyone only talked about. (The Claude Code CLI mode, which has no custom tools, falls back to hidden tags.)
- **Compressed facts.** Facts are short and labelled (`Origin: …`, `Job: …`, `Likes: …`), so profiles stay small and searchable.
- **Unlimited, searched on demand.** There is no cap on facts. Instead of stuffing every profile into the prompt, the model gets a `search_facts` tool backed by SQLite FTS5. It can search by handle, by name, or by fact text across everyone it knows. When nothing matches a named person, it falls back to that person's newest facts.
- **Name disambiguation.** People who share a name get an ID tag (`Omar #9249`, `Omar #1180`), which the model can use to read or write the right profile.
- **Per-chat memories.** Separately, the model can tag important chat context (`plan`, `decision`, `preference`, …), which is retrieved by keyword when the recent window doesn't have it.
- **Self-updating.** A background curator reads every chat the bot is in (not just messages addressed to it) every 16 messages or after 10 quiet minutes, and saves new facts and nicknames on its own. Nicknames link back to real profiles and merge stray name-only ones.
- **Facts with provenance.** Every fact records who said it and how much that source is trusted: the owner, the person themselves, someone else in the chat, or the bot's own inference. Hearsay ranks lower and is shown as unverified, and a less trusted source can't overrule a more trusted one.
- **Facts age instead of vanishing.** Forgetting or correcting marks a fact as no longer true and keeps it as history. Short-lived states ("on the way home") expire after a day. Owners can retire anyone's facts; people can retire their own.
- **One profile per person.** A profile made from someone's number merges into their account once WhatsApp reveals their LID, and a profile made from a name ("Andi") merges into the real account ("Andi Pratama") when exactly one matching person is in the chat where it was learned.
- **Search that understands paraphrase.** Fact search combines keywords with embeddings, so "what does he do for work" finds "Job: IT engineer". Embedding a fact costs a fraction of a millionth of a dollar.
- **Long chats keep their shape.** Only the last 64 messages go to the model, so the curator also keeps a rolling summary of each chat (who is who, ongoing plans, running jokes, and who holds which side of an open argument), at most 300 words, shown before the transcript. Messages older than that window are kept for 72 hours, and the model can search them with a `chat_history` tool (this chat only).
- **Reminders and nags.** "Remind me to eat at 12" schedules a message; owners can make it repeat (hourly, daily, weekdays, weekly) or send it to another group they are in, and the bot may schedule a caring nag on its own, rarely. Jobs are stored, so they survive restarts. Ask "what reminders do you have?" to list or cancel them. Each reply also sees the chat's next reminders with the time left worked out by the server, so "how long until" is exact.
- **No spam pile-ups.** A fact that nearly repeats one already stored overwrites it instead of adding another row, so someone spamming the same bait leaves one fact, not fifty.

### Persona system

- **One global persona.** The bot has a single character (name, look, voice, quirks), stored in SQLite with full version history.
- **Changed in plain language.** An owner says `@Rin from now on be more cheerful`, and the model writes the complete merged persona back in a hidden `<persona>` tag. Requests from non-owners are refused in character and logged.
- **Fixed rules come first.** Pure chat only, WhatsApp formatting, chat content is data, and no fake memories always outrank the persona.
- **Immersion rules.** The model stays in character, never "breaks the fourth wall" about its own body or surroundings, never narrates its reasoning, and knows the current local time.

### API and MCP tools

- **`http_request`:** call any public HTTP API. Big responses (like a 400 KB OpenAPI file) can be searched with `find` or paged with `offset`, so the model reads an app's API docs instead of guessing. Pages that only render with JavaScript are read through the app's API instead.
- **MCP client:** list servers in `data/mcp.json` (the same format as Claude Desktop / Claude Code), local commands or remote URLs, and their tools become available to the model as `mcp__<server>__<tool>`. Uses the official MCP Go SDK.
- **Stored API keys:** owners save a key once, with `/key <host> <token>` in a DM to the bot or in `data/api_keys.json`, and `http_request` adds it to https requests to that host for owners only. The model never sees the key.
- **Who can use them:** anyone who can trigger the bot can use `http_request`; MCP tools and stored keys are for whitelisted owners only. `http_request` refuses loopback, private, link-local and metadata addresses at connect time, so nobody can reach the server's own network through it.

```json
{"mcpServers": {
  "github": {"url": "https://api.githubcopilot.com/mcp/", "headers": {"Authorization": "Bearer <token>"}},
  "files":  {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/srv/share"]}
}}
```

### Chat behaviour

- **Groups:** anyone can trigger it by **@mentioning** the bot or **replying** to one of its messages. It reads the last N messages as context, including quoted replies and images.
- **DMs:** whitelisted numbers only. Strangers get a fixed denial, without calling the model, and the denial itself is rate-limited.
- **Group rules:** the bot only stays in groups that a whitelisted member added it to and that still contain one; otherwise it leaves.
- **Flood control:** someone who sends 10 messages in 2 minutes gets a one-word check by the model (spam or fast chat); spam earns a 5-minute timeout in every chat, and their messages are dropped before any model call. Model spend is capped at $2 a day; past it, only owners get answers.
- **Ban-safe pacing:** all outgoing messages are globally spaced (default 1 every 3 s) and queued, never dropped.
- **Web:** web search and link reading run as server-side tools on the provider.
- **Owner commands:** handled by the bot itself, never stored or sent to the model. `/reset` clears the chat's transcript and summary for a fresh start (facts, memories and reminders stay). `/key` manages stored API keys, in a DM only. `/mute @person [30m]` and `/unmute @person` silence someone in every chat; `/mute` lists who is muted. Commands from anyone else are ignored.
- **Prompt-injection hardening:** chat text, names and quotes are neutralised so they can't close or fake the prompt's sections. Owners are identified by WhatsApp ID, not by what a message claims.

### Model providers

| `PROVIDER` | Auth | Notes |
|---|---|---|
| `anthropic` (default) | `ANTHROPIC_API_KEY` | Claude Messages API with tool loop, server-side web search/fetch, image input |
| `anthropic` | `CLAUDE_CODE_OAUTH_TOKEN` | Claude Code CLI in headless mode (subscription), all local tools off |
| `openrouter` | `OPENROUTER_API_KEY` | Any OpenRouter model (e.g. DeepSeek), OpenRouter web tools, image fallback for text-only models |

Each request's input is capped (default 128k tokens). Old transcript lines are trimmed first, then memories, then the request itself.

## Quick start

```sh
cp .env.example .env      # fill in a provider key and WHITELIST
go run .                  # scan the QR (or set PAIR_NUMBER for a pairing code)
```

Use a spare number, not your personal one. The bot sends real messages.

Build for a Linux server:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o wagoya .
```

`deploy/` has a systemd unit and a deploy script (cross-compile, scp, restart).

## Configuration

All settings live in `.env`; see [`.env.example`](.env.example) for every key. The main ones:

| Key | Default | Meaning |
|---|---|---|
| `PROVIDER` | `anthropic` | `anthropic` or `openrouter` |
| `MODEL` | `claude-haiku-5-5` / `deepseek/deepseek-v4.1-flash-1017` | Model ID for the chosen provider |
| `EFFORT` | `low` | Reasoning effort |
| `WHITELIST` | | Owner phone numbers (resolved to LIDs) |
| `BOT_NAME` | `Claude` | Name used in prompts and mentions |
| `HISTORY_LIMIT` | `64` | Recent messages per chat sent as context |
| `CONTEXT_TOKENS` | `128000` | Input cap per request |
| `WEB_TOOLS` | `true` | Web search and link reading |
| `API_TOOLS` | `true` | `http_request` (everyone) and MCP tools (owners) |
| `MCP_CONFIG` | `data/mcp.json` | MCP servers file |
| `EMBED_MODEL` | `openai/text-embedding-3-small` | Embedding model for hybrid fact search, via OpenRouter (needs `OPENROUTER_API_KEY`); `off` for keywords only |
| `SEND_INTERVAL` | `3s` | Global gap between outgoing messages |
| `DENY_COOLDOWN` | `1m` | Per-chat cooldown for the denial reply |

## Layout

```
main.go             WhatsApp events, identity, triggers, group rules, wiring
internal/router     trigger decisions, prompt building, hidden-tag parsing, token budget
internal/store      SQLite: chat window, memories, persona history, people and facts (FTS5)
internal/llm        model clients: Claude API, Claude CLI, OpenRouter; generic tool loop
internal/memtools   memory tools: search_facts, save_fact, invalidate_fact, save_memory
internal/tools      http_request (public-only) and the MCP client
internal/ratelimit  global send pacer and per-chat cooldown
internal/config     .env loading and validation
```

Design notes: [`ARCHITECTURE.md`](ARCHITECTURE.md). Every design choice, with its reason: [`DECISIONS.md`](DECISIONS.md). Plan: [`ROADMAP.md`](ROADMAP.md).

## Tests

```sh
go test ./...                           # offline
LLM_LIVE=1 go test ./internal/llm -v    # real model calls (needs keys, costs money)
```

## Data and privacy

`.env` and `data/` (the WhatsApp session, chat window, memories and people profiles) are secret and git-ignored. Profiles are global by design, so facts learned in one chat can surface in another. Run it only where everyone is fine with that.

Unofficial: wagoya uses WhatsApp's multi-device protocol via whatsmeow and is not affiliated with WhatsApp or Meta. Automated accounts can be banned; use a spare number.
