# wagoya

How the system is built. `DECISIONS.md` overrules this file wherever they disagree. Cite decision codes (`B2`) instead of restating the choice or its reason.

```mermaid
flowchart LR
    U["User / group members"] <--> WA["WhatsApp servers"]
    WA <--> Bot["Bot process (VPS, systemd)"]
    Bot --> C["Model: Claude Messages API, claude CLI, or OpenRouter"]
    Bot --> D[("data/: session.db + bot.db")]
```

## A. Purpose and scope

A WhatsApp number backed by Claude that the user can DM and add to groups (A1).

**Goals**
- Reply in groups when anyone @mentions the bot or replies to it (B1, B3), using recent group messages and stored memories as context (B2, B5).
- Reply to whitelisted DMs and deny everyone else without calling Claude (B4, B7).
- Stay only in groups added by, and containing, a whitelisted member (B15).
- Run unattended on the VPS (A4, C1).
- Play one global persona that only whitelisted users can change, and remember people by LID across chats (D1-D4).

**Non-goals**
- Generating media or files (B8). Tools that act on the VPS from chat input.

## B. Context

| Actor / external system | Direction | Interface |
|---|---|---|
| Whitelisted users | both | WhatsApp DM and groups, identified by LID (B9) |
| Other group members | in (context, triggers), out (replies) | WhatsApp groups |
| WhatsApp servers | both | Linked-device protocol via whatsmeow (A2) |
| Model | out | `anthropic-sdk-go` Messages API, or `claude -p` headless with no tools (A5, A7), or OpenRouter chat completions over `net/http` (G1, G2) |

## C. Components

| Component | Code | Responsibility | Owns |
|---|---|---|---|
| Connection | `main.go` | Link (B10), connect, LID resolution (B9), group membership rules (B15), send replies | `data/session.db` |
| Router | `internal/router` | Pure logic: reply/store/deny decision (B1, B3, B4, B7), handles P1..Pn (D4), prompt building with tag neutralising (B9), system prompt with persona (B8, D1), hidden-tag parsing (B12, D2, D4), 128k fitting (B17) | — |
| Store | `internal/store` | Last 32 messages per chat (B5, B11); per-chat memories with FTS5 (B12, B16); persona with history (D1); global people profiles and facts (D3) | `data/bot.db` |
| Rate limits | `internal/ratelimit` | Global 3 s message pacing with queue (B18); per-chat denial cooldown (B14) | in-memory |
| LLM | `internal/llm` | One stateless request, text + optional image (B6, B8); refusals passed through (B13) | `data/claude-work/` (CLI cwd) |
| Config | `internal/config` | `.env` loading and validation | — |

Each triggered request runs in its own goroutine, serialised per chat.

## D. Key flows

**Group message, not triggered**: stored in the chat window (B2). No reply.

**Group message that @mentions the bot or replies to it** (any member, B3)
1. Load the chat window, store the message, search memories with the prompt text (top 8).
2. Download the image if the message has one (B8).
3. Assign handles P1..Pn (requester is P1). API mode: show each person's fact count and give the model a `search_facts` tool (FTS5 over every known person, by handle, name or fact text); CLI mode: put all their facts inline (D7). Build the prompt: `<people>`, `<memories>`, `<transcript>`, `<request>`; whitelisted senders are marked by LID/number, not name. The system prompt carries the current persona.
4. Call Claude. Strip hidden tags and apply them: `<memory>` → chat memories, `<person id>` or `<person name>` → that person's global profile (any known person, or a new name-only profile), `<forget id>` and `<persona>` → only if the requester is whitelisted (B12, D2, D4).
5. Send the reply quoting the trigger; store the reply in the window.

**DM**: whitelisted → same as a triggered message, no prefix needed (B4). Otherwise → fixed denial text, nothing stored (B7, B14).

**Added to a group**: leave unless the adder is whitelisted and a whitelisted member is present. Also leave when the last whitelisted member leaves, and at startup for any group without one (B15).

## E. Data

| Entity | Stored in | Lifetime |
|---|---|---|
| WhatsApp session | `data/session.db` (whatsmeow sqlstore) | Until logout. Secret (C3). |
| Chat window | `data/bot.db` `messages` | Last `HISTORY_LIMIT` (32) per chat (B5, B11) |
| Memories | `data/bot.db` `memories` + `memories_fts` | Permanent (B16) |
| Persona | `data/bot.db` `persona` (one row per change) | Permanent; latest row is current (D1) |
| People | `data/bot.db` `people` (LID, number, name) + `person_facts` + `person_facts_fts` | Permanent, uncapped, global across chats (D3, D7) |

## F. Interfaces and contracts

| Interface | Kind | Spec location |
|---|---|---|
| Configuration | env file | `.env.example` |
| Group trigger | chat syntax | `@<bot> <prompt>` mention, or a reply to the bot (B1) |
| Hidden tags | Claude output | `<memory category="…">`, `<person id="P2">`, `<forget id="P2">`, `<persona>` (B12, D2, D4) |

## G. Stack

| Need | Choice | Decision |
|---|---|---|
| Language | Go | A6 |
| WhatsApp | `go.mau.fi/whatsmeow` | A2 |
| SQLite | `modernc.org/sqlite` (pure Go, FTS5) | C4 |
| LLM | `anthropic-sdk-go/v2` or `claude` CLI, `claude-haiku-5-5` (default), effort low | A5, B6 |
| Process manager | systemd (`deploy/wagoya.service`) | C1 |
| Deploy | `deploy/deploy.sh`: cross-compile, scp | C2 |

## H. Quality targets

| Attribute | Target | How checked |
|---|---|---|
| Reply latency | Proposed: under 15 s for a short prompt | Manual timing in a test group |
| Uptime | Restarts and reconnects by itself | `sudo reboot`, then DM the bot |
| Memory | Proposed: under 150 MB RSS (VPS has 2 GB) | `systemctl status wagoya` |

## I. Constraints

- No heavy compute on the user's laptop. The laptop only develops and deploys.
- The bot must use a dedicated number because of the ban risk (A3).

## J. Repository layout

```
main.go             WhatsApp wiring
internal/router     pure decision and prompt logic (tested)
internal/store      SQLite window + memories (tested)
internal/llm        model clients (Claude API, Claude CLI, OpenRouter)
internal/tools      http_request and MCP client (I1-I3)
internal/ratelimit  message pacing, denial cooldown (tested)
internal/config     .env loading
deploy/             systemd unit, deploy script
data/               runtime state (gitignored, secret)
```
