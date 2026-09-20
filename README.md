# Bifrost

> **OpenAI-compatible API gateway over Gemini Web — use the best free AI models in any coding agent, no API key required.**

Bifrost controls a Chromium browser via Chrome DevTools Protocol (CDP) and exposes a standard OpenAI-compatible HTTP API. Point any tool that speaks the OpenAI protocol — [OpenCode](https://opencode.ai), [Cursor](https://cursor.sh), [Aider](https://aider.chat), [Continue](https://continue.dev), Claude Code, and more — at Bifrost and use Gemini Web for free.

```
Your coding agent ──► POST /v1/chat/completions ──► Bifrost ──► Chromium CDP ──► gemini.google.com
                   ◄── OpenAI-compatible JSON ◄──          ◄── DOM extraction ◄──
```

## Why Bifrost?

| | Bifrost | Direct API |
|---|---|---|
| Cost | **Free** (uses your Google account) | Paid per token |
| Models | Gemini Pro, Flash, Flash Lite | Same |
| Setup | `docker compose up -d` | API key + billing |
| Streaming | ✅ real-time (DOM observer, first token < 1s) | ✅ |
| Function calling / tools | ✅ (simulated, with auto-retry) | ✅ (native) |
| Multi-turn conversation | ✅ sticky (context cached web-side, only the delta is sent) | ✅ |
| Rate limits | Google's web limits | API tier limits |

---

## Quick Start

### Prerequisites

- Docker + Docker Compose
- A Google account logged into Gemini (or do the login step below)
- An X11 display available (for the login step — `echo $DISPLAY` should return `:0` or similar)

### 1. Clone and start

```bash
git clone https://github.com/yourname/bifrost.git
cd bifrost
docker compose up -d --build
```

### 2. Log in to Gemini

The browser profile is empty on the first run. Open the panel and trigger a login:

```
http://localhost:8081
```

Click **"Nova sessão"** (always visible in the header; the banner shows "Reconectar sessão" when the session is missing) — a Chrome window opens on your desktop. Log in with your Google account; Bifrost detects the session, closes the window itself, saves the profile and resumes headless. The login progress is broadcast live in the panel — anyone with it open sees the state.

Alternatively, use the CLI:

```bash
# Run outside Docker (needs a local Go install):
go run . login
```

### 3. Verify

```bash
curl http://localhost:8081/health
# {"status":"ok"}

curl http://localhost:8081/v1/models
# lists available Gemini models
```

### 4. Connect your agent

Set these in your agent's settings:

| Setting | Value |
|---|---|
| Base URL | `http://localhost:8081/v1` |
| API Key | `any` (or leave blank; set `BIFROST_API_KEY` for auth) |
| Model | `gemini-web` (Pro) or `gemini-web-flash`, `gemini-web-flash-lite` |

---

## Available Models

| Model ID | Gemini model | Best for |
|---|---|---|
| `gemini-web` | Current UI mode (Pro by default) | Complex tasks, code generation |
| `gemini-web-pro` | Pro | Deep reasoning, code |
| `gemini-web-pro-extended` | "Raciocínio complexo" | Hardest problems |
| `gemini-web-flash` | Flash | Balance of speed and quality |
| `gemini-web-flash-lite` | Flash Lite | Fast, simple tasks |

> Run `bifrost modes` after a Gemini UI update to re-map menu items to labels and keep this registry honest.

---

## Configuration

All settings are environment variables:

| Variable | Default | Description |
|---|---|---|
| `BIFROST_ADDR` | `:8080` | HTTP listen address |
| `BIFROST_PROVIDER` | `gemini` | LLM provider: `gemini` or `chatgpt` (experimental) |
| `BIFROST_PROFILES` | *(none)* | Multi-profile: comma-separated Chrome profiles — one browser per account, round-robin (see below) |
| `BIFROST_POOL_SIZE` | `1` | Tabs per profile (with sticky conversations, 1 is recommended — affinity is per profile) |
| `BIFROST_PROFILE` | `./data/chrome-profile` | Chromium user data dir (persists login) |
| `BIFROST_HEADLESS` | `false` | Run Chrome headless (set to `true` after login) |
| `BIFROST_CHROME` | *(auto)* | Path to Chromium binary |
| `BIFROST_API_KEY` | *(none)* | If set, requires `Authorization: Bearer <key>` |
| `BIFROST_LOG` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `BIFROST_MODEL` | `gemini-web` | Default model when request omits `model` field |
| `BIFROST_PASSWORD_STORE` | `gnome-libsecret` | Chrome cookie encryption: `gnome-libsecret` (desktop) or `basic` (container) |
| `BIFROST_NO_SANDBOX` | `false` | Add `--no-sandbox` flags (required in Docker) |
| `BIFROST_TOOL_RESULT_MAX` | `8192` | Max chars per tool result in the serialized prompt (fat bash/file outputs get head+marker+tail elision). `0` disables |
| `BIFROST_MAX_PROMPT` | `150000` | Global prompt budget in chars. Over it, content is squeezed in stages — old tool results → old tool-call **arguments** (write/edit calls carry whole files) → old assistant/user text — and, as a last resort, the recent working set. System messages and the tool protocol are never elided. Still over after everything: `400 context_length_exceeded` instead of wedging the page. `0` disables |

Example `docker-compose.yml` with auth:

```yaml
services:
  bifrost:
    build: .
    ports:
      - "8081:8080"
    volumes:
      - ./data/chrome-profile:/data/chrome-profile
      - /tmp/.X11-unix:/tmp/.X11-unix
    environment:
      DISPLAY: "${DISPLAY:-:0}"
      BIFROST_API_KEY: "your-secret-key-here"
      BIFROST_LOG: "info"
    shm_size: "1gb"
    restart: unless-stopped
```

---

## API Reference

Bifrost implements a subset of the OpenAI Chat Completions API.

### `GET /health`

Returns server and browser status.

```json
{"status": "ok"}
```

Status can also be `{"status": "degraded", "browser": "down"}` or `{"status": "degraded", "session": "missing"}`.

### `GET /v1/models`

Returns the list of available models in OpenAI format.

### `POST /v1/chat/completions`

Standard OpenAI chat completions endpoint. Supports:

- `messages` — full conversation history
- `model` — model ID (see table above)
- `stream` — `true` for SSE streaming, `false` for batch
- `tools` — function definitions (simulated, see below)
- `tool_choice` — `"auto"`, `"required"`, or `{"type":"function","function":{"name":"..."}}`
- `parallel_tool_calls` — `false` instructs the model to emit at most one call per response
- `function.strict: true` — instructs exact-schema adherence (instructed, not guaranteed: Gemini Web has no native tool protocol)

**Request:**

```json
{
  "model": "gemini-web",
  "stream": true,
  "messages": [
    {"role": "user", "content": "What is the capital of France?"}
  ]
}
```

**Response (non-stream):**

```json
{
  "id": "chatcmpl-bifrost-...",
  "object": "chat.completion",
  "model": "gemini-web",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": "Paris."},
    "finish_reason": "stop"
  }],
  "usage": {"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}
}
```

---

## Function Calling (Tools)

Gemini Web has no native tool-calling protocol. Bifrost simulates it:

1. Tool schemas + a calling protocol are injected as a system message in the prompt
2. The model emits tool calls as JSON code blocks:
   ````
   ```
   {"name": "tool_name", "arguments": {"key": "value"}}
   ```
   ````
3. Bifrost extracts the calls and returns them in the standard OpenAI `tool_calls` format
4. If the model returns text instead of calling a write tool, Bifrost auto-retries with a correction

**Supported patterns:**
- Single and parallel tool calls — including multiple JSON objects inside one code block
- `tool_choice: "required"` (model is instructed to always call a tool)
- `tool_choice: {"type":"function","function":{"name":"..."}}` (specific tool)
- `parallel_tool_calls: false` (at most one call per response)
- `function.strict: true` (exact-schema adherence, instructed)
- Multi-turn tool use (tool results passed as `tool` role messages), with an anti-repeat instruction when results are already in the history
- `content` as string, `null`, or multi-part array (`[{"type":"text","text":"..."}]`) — modern clients and bridges work out of the box
- Auto-retry with correction when the model: refuses to use tools (two-step ladder in stream and non-stream), answers with prose instead of calling a write tool, or emits the call as loose JSON instead of a code block
- Robust parsing: literal newlines repaired, stringified arguments unwrapped, `{"tool_call": {...}}`-style wrappers unwrapped, calls scanned outside code blocks as a last resort
- Exact-duplicate calls within one response are dropped (the classic Gemini Web repeat bug)

**Prompt protocol:** tool schemas are rendered with their required parameters listed explicitly, and the one-shot example uses a real tool from the request (with type-correct placeholder arguments synthesized from its schema) — including a dedicated wrong-vs-right example for write tools citing the actual write tool name.

**Streaming behavior:** tool-call-shaped code blocks (JSON starting with `{`) are withheld from the content stream and translated to `delta.tool_calls` — **emitted early**, as soon as a call block closes and stabilizes mid-generation (the client can start executing the first call while the rest of the response still generates), with the remainder translated at the end. Index assignment is shared between early and final emission; already-emitted calls are never re-sent, and call blocks never leak as text.

**Known limitations:**
- Success rate varies (~85-95% without retries; the auto-retry ladder recovers most failures) — the model sometimes ignores tool instructions
- Token counts are estimated, not exact

**Prompt budget (wedge protection):** very long agent sessions (100+ messages, 28 tools) produce prompts that wedge the Gemini page's renderer for minutes — every CDP evaluate queues behind it, and before the fix even `/health` and the panel hung together (they probed the page without a deadline). One such session reached **244 KB** (tool-call fences carrying whole files in their arguments + agent system prompt + 28-tool protocol). Protection layers:

1. **Always-on caps**: per tool result (`BIFROST_TOOL_RESULT_MAX`, 8192), per user/assistant message (12 KB — giant user pastes), per tool-call arguments (4 KB — `write`/`edit` calls carry whole files; over the cap the arguments become a valid-JSON marker). All deterministic — the same resent history elides identically, so sticky prefix matching between turns is preserved. System messages and the tool protocol are never elided (agent identity).
2. **Global budget** (`BIFROST_MAX_PROMPT`, default 150000 chars): over it, content is squeezed in stages — old tool results → old tool-call arguments → old assistant/user text (head-only, oldest first, last 6 messages spared) — and, as a last resort, the recent working set is degraded too (worse recent context beats an error). Still over after everything: **`400 context_length_exceeded`** — an honest error the client can act on, instead of typing 244 KB and wedging the page.
3. **Escalating unwedge ladder**: state probes carry short ceilings (30s pre-submit, 5s for health/panel — a wedged page fails fast instead of eating the request's 3-minute deadline, and `/health`/`/panel/data` report the shard as down instead of hanging). A wedged page is recovered in escalating steps: **tab reload** (browser-level command, replaces the hung renderer, preserves the conversation) → **fresh page via /app** (abandons the giant conversation — reloading merely re-rendends it and it re-wedges; the next turn retypes the bounded history) → **browser relaunch** (when even navigation doesn't respond, the Chrome process itself is dead; the shard is marked and the next acquire restarts it, recreating workers from the persisted profile session). If typing wedges *mid-prompt*, the sequence restarts cleanly on a fresh page with the full bounded history.

---

## Sticky Conversations (Memory)

Agents resend the **full history** on every turn. Bifrost detects that the new history is a continuation of the previous one (prefix match + same model + same response count) and **reuses the same Gemini conversation**, sending only the delta:

```
Turn 1: [system, user]                                    → full prompt (fresh conversation)
Turn 2: [system, user, assistant(tool_call), tool(result)] → only "[TOOL result] + protocol reminder"
```

Benefits:

- **Speed** — each turn's prompt stays tiny; the history stays cached on Google's side
- **Context fidelity** — the model sees the actual conversation, not a re-serialization of it
- **Retry quality** — when a response is refused/malformed, the correction is appended to the *same* conversation: the model sees its own bad answer plus the fix
- **Replay safety** — an identical resend (client retry) returns the cached answer instead of generating a duplicate

Guard rails: a conversation reopens (fresh, full history) after 30 responses, on model change, when someone typed in the UI meanwhile, or when the last turn failed with uncertain state.

> Note: with `BIFROST_POOL_SIZE > 1`, consecutive turns may land on different tabs **within the same profile** and fall back to a fresh conversation (graceful degradation). Cross-profile, the conversation key routes turns to the profile that owns the conversation — sticky survives multi-profile.

---

## Multi-Profile (Multiple Accounts)

Gemini Web usage limits are **per account**. `BIFROST_PROFILES` runs one browser per Google account and rotates requests across them — N accounts ≈ N× the capacity (and N× the Pro quota):

```yaml
# docker-compose.yml
volumes:
  - ./data/chrome-profile:/data/chrome-profile
  - ./data/chrome-profile-2:/data/chrome-profile-2
environment:
  BIFROST_PROFILES: "/data/chrome-profile,/data/chrome-profile-2"
```

How it behaves:

- **Round-robin with conversation affinity** — consecutive turns of the same agent conversation return to the profile that owns it (sticky conversations survive multi-profile)
- **Dead profiles self-heal out of the rotation** — a profile without a session (or with a dead browser) is skipped; requests never queue behind it
- **Per-profile login from the panel** — each profile gets its own "conectar" button; the login of one profile doesn't interrupt the others
- **Warm-up pre-marks empty profiles** — the first request never hits a cold 503; `/health` reports `{"profiles":"1/2","status":"ok"}`
- **Disabled modes degrade per request** — when one account hits its Pro/Flash limit, its menu items go disabled; Bifrost logs the reason and serves from the current mode instead of failing

> Keep one profile per Google account; duplicate paths collapse into one shard.

---

## Web Panel

Bifrost includes a built-in monitoring panel at `http://localhost:8081/panel`:

- Live request log (model, status, chars, tool calls, retries, duration) with expandable prompt/response
- Real-time log stream (slog tee)
- Browser/session/queue health cards
- **Session creation from the panel** — the "Nova sessão" button (always visible) opens a headed Chrome window for login; progress is broadcast to every open panel; the window closes itself after the session is confirmed and the gateway resumes headless automatically

---

## CLI Commands

```bash
bifrost login     # Open Chrome for manual Google login
bifrost inspect   # Dump Gemini DOM as JSON (+ screenshot) for selector debugging
bifrost modes     # Enumerate and test all available Gemini modes
bifrost test "your prompt here"   # End-to-end test: send a prompt and print the response
bifrost serve     # Start the HTTP API server (default when no command given)
```

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                         bifrost                             │
│                                                             │
│  HTTP (chi router)                                          │
│  ├── /health, /panel, /panel/*                              │
│  └── /v1/chat/completions  ──► handleChat()                 │
│                                    │                        │
│                             Gateway (supervisor)            │
│                             ├── ProviderFactory (gemini/    │
│                             │   chatgpt)                    │
│                             ├── tab pool (POOL_SIZE)        │
│                             ├── queue (max 4 waiting)       │
│                             └── auto-restart + panel login  │
│                                    │                        │
│                             Gemini (CDP controller)         │
│                             ├── sticky conversation state   │
│                             ├── typePrompt() → InsertText   │
│                             ├── MutationObserver → chunks   │
│                             │   (console → CDP → channel)   │
│                             └── genStateJS() → extraction   │
│                                    │                        │
│                          Chromium (chromedp/CDP)            │
│                          └── gemini.google.com              │
└─────────────────────────────────────────────────────────────┘
```

**Key design decisions:**

- **Sticky conversations** — consecutive turns reuse the same Gemini conversation and send only the delta; the history stays cached web-side (see [Sticky Conversations](#sticky-conversations-memory))
- **Observer-driven streaming** — a `MutationObserver` pushes response text through the CDP console channel (~100ms throttle); a 300ms DOM poll remains as completion detector and fallback
- **CDP over Puppeteer/Playwright** — pure Go, no Node.js dependency, smaller Docker image
- **Profile persistence** — Chrome saves the Google session to disk; no re-login on restart
- **Provider abstraction** — `ProviderFactory`/`LLMWorker` interfaces; ChatGPT web is an experimental second provider (`BIFROST_PROVIDER=chatgpt`)
- **Structured DOM extraction** — the response is extracted as typed parts (text, code, tables, nested lists), not raw `innerText`: code blocks keep language label + pure code, tables become markdown pipes, nested lists keep indentation, inline `code`/`bold`/`italic` keep their markers — all via a recursive walk that pierces the UI's wrapper divs

---

## Building from Source

```bash
git clone https://github.com/yourname/bifrost.git
cd bifrost

# Run directly (needs Chromium in PATH)
go run . serve

# Build binary
go build -o bifrost .

# Build Docker image
docker build -t bifrost:latest .
```

**Requirements:** Go 1.26+, Chromium/Chrome

---

## Troubleshooting

### Session expired / Gemini redirects to login

Run `bifrost login` or click "Iniciar Login" in the panel. The Chrome profile persists the session — expiry happens when Google invalidates the session token (typically weeks/months).

### `profile already in use` error

Another process holds the Chrome profile lock. Run:

```bash
pkill -x bifrost   # kill any running instance
# or
docker compose restart
```

### Response is empty / timeout

1. Run `bifrost inspect` to dump the current DOM and check that selectors still match the Gemini UI
2. Run `bifrost test "hello"` to test end-to-end
3. Check logs: `docker compose logs -f`
4. The Gemini UI updates frequently — if selectors break, open an issue with the `inspect` output

### Function calls not being executed by my agent

Ensure your agent is sending tool definitions in the `tools` field of the request. Bifrost only activates the tool-calling protocol when tools are declared. If the problem persists, check the panel for `retry` events — Bifrost auto-retries when the model ignores tool instructions.

---

## Limitations & Known Issues

- **Single profile** — one Chromium/Google account; the tab pool parallelizes conversations but shares rate limits
- **Fragile selectors** — the Gemini UI is not a public API; Google may change it at any time
- **No image input** — multimodal (vision) is not yet implemented (roadmap #1)
- **Estimated token counts** — usage stats are approximations (chars/4)
- **Linux/macOS only** — the login flow requires X11/Wayland for headed Chrome
- **Google ToS** — using Gemini Web programmatically may violate Google's Terms of Service; keep it personal, don't resell or share publicly

---

## Roadmap — next steps

Ordered by impact for the main use case (agentic coding with a Gemini Pro subscription):

1. **Image input** — translate OpenAI `image_url` content parts into composer file uploads via CDP. Unlocks *screenshot → landing page* and mockup-driven generation.
2. **Automatic model routing** — flash-lite/flash for trivial steps (file reads, summaries), pro for code-heavy turns; either per-request heuristics or a tiny classifier.
3. **Per-client API keys** — multiple keys with per-key usage in the panel; share the gateway with a small team without exposing your account.
4. **Cross-tab sticky affinity** — route consecutive turns to the *tab* that owns the conversation (today affinity is per profile; `POOL_SIZE > 1` within a profile degrades to fresh conversations).
5. ~~**Multi-profile pool**~~ — **done**: `BIFROST_PROFILES` runs one browser per account in round-robin with conversation affinity (see [Multi-Profile](#multi-profile-multiple-accounts)).
6. **Sticky state persistence** — store conversation URLs + history hashes on disk so agent sessions survive gateway restarts.
7. **Observer-only completion detection** — silence-based generation-end detection to retire most of the 300ms polling.

---

## Contributing

Contributions welcome! Particularly useful areas:

- **Selector updates** — if the Gemini UI changed and broke something, run `bifrost inspect` and open a PR updating `geminiSelectors` in `gemini.go`
- **New model support** — run `bifrost modes` to list current modes and update the model registry
- **Stability improvements** — the CDP interaction is inherently fragile; better heuristics and retry logic are always welcome
- **Vision/multimodal** — attaching images to the Gemini Web prompt via CDP

Please open an issue before large changes.

---

## License

MIT — see [LICENSE](LICENSE).

---

*Bifrost: the rainbow bridge between your tools and Gemini.*
