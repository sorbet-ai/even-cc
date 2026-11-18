# Even Terminal wire protocol (phone app ↔ host server), Claude-only

This is the contract between the Even Realities phone app ("Terminal Mode", G2 glasses) and the host
server `@evenrealities/even-terminal`. It's written so we can build a drop-in Go replacement
(`even-cc`) that serves **Claude Code only**.

**Sources:**

- **Code:** `@evenrealities/even-terminal` **0.10.5**, compiled JS in
  `~/work/projects/even-terminal-warm`. The `upstream` commit `28d12b4` is the published package.
  The `warm` branch changes only `dist/claude/session.js` (it keeps a live process between prompts).
  Where the two differ on the wire, both behaviours are given. The bundled
  `@anthropic-ai/claude-agent-sdk` is **0.3.314**.
- **Logs:** `~/work/even-terminal-*.log`, 2026-10-28 → 2026-10-30. They hold real phone traffic from
  `192.168.1.60` at debug level: every SSE payload, every HTTP status line, and the bodies of
  permission and question answers.
- **Live captures:** read-only `GET`s, plus `POST`s that fail validation and so cause no side effects,
  sent on 2026-10-30 to the running server (`127.0.0.1:3456`, the warm build). HTTP response bodies
  aren't in the logs, so every response body here comes from these captures.

**Tags:**

- **[inferred]** means not seen directly in code or logs.
- **[not observed]** means the code emits it, but it never appears in the logs.
- **[warm]** and **[upstream]** mark behaviour that differs between the two builds.

---

## 0. TL;DR: what the phone actually does (from logs)

| Call | Seen | Notes |
|---|---|---|
| `GET /?token=…&defaultProvider=claude` | 2× | Only right after pairing (QR scan). Gets Express's HTML 404, and the app carries on. |
| `GET /api/info` | 582× | Polled every **10 s**. No query params. |
| `GET /api/sessions?provider=claude` | 584× | Polled every **10 s**, 5 s out of phase with `/api/info`. |
| `GET /api/sessions?provider=codex` | 633× | Polled in the same cycle. The Go server must still answer it (§3.3). |
| `GET /api/events?token=…&sessionId=<id>` | 17× | SSE. The token is in the **query string**. The app never sends `needReplay` or `after`. |
| `GET /api/sessions/<id>/history?limit=10&provider=claude` | 19× | Sent right after each SSE connect, within ~1 s. |
| `POST /api/prompt` | 20× (202) | Always carries an existing `sessionId`. The app never created a new session in the logs. |
| `POST /api/permission-response` | 2× | `{"sessionId","decision":"allow","provider":"claude"}` |
| `POST /api/question-response` | 1× | `answer` is a JSON **string** that encodes an object |
| `/api/status`, `/api/messages`, `/api/interrupt`, `/api/update-check`, `/api/metrics`, `/api/debug/*` | 0× from the phone | Implement them anyway; see §3. |

All REST calls authenticate with the `Authorization: Bearer` header. The logged URLs have no
`token=REDACTED`, and the query param is the only other way in. Only SSE uses `?token=`.
**[inferred from logs]**

---

## 1. Startup and pairing

### 1.1 QR code / pairing URL

```
http://<address>:<port>?token=<token>&defaultProvider=<provider>[&name=<name>]
```

- It's built by `buildClientQuery` with `new URLSearchParams({token, defaultProvider})`, then
  `name` is appended if set. Form encoding applies, so a space becomes `+`.
- There's **no `/` before `?`**. Example: `http://192.168.1.85:3456?token=1d57…&defaultProvider=claude`.
- `defaultProvider` comes from `DEFAULT_PROVIDER` (the config's `provider`). For even-cc it's always `claude`.
- `name` comes from `EVEN_TERMINAL_NAME` (config `name` or `--name`). It's left out when empty.
- The QR code, and the URL line above it, are printed only when the host address is non-empty
  (LAN, Tailscale or interface mode). The banner also prints `Full token: <token>`.
- In expose mode (pinggy, bore, ngrok), the QR encodes `<public https URL>?token=…&defaultProvider=…`,
  so the app accepts `https://` base URLs too.
- On first contact, the app GETs that exact URL (path `/`). The server answers with Express's default
  404 HTML (`testdata/even/http/root.404.html`, `Content-Type: text/html; charset=utf-8`). It then
  goes on to `/api/info`. A 404 here is fine. What the app does with any other answer is
  **[inferred]**: unknown, so keep the 404.

### 1.2 Bind addresses and ports

- **Port:** `PORT`, default **3456**. It comes from config `port`, overridden by `--port`.
- **Host resolution** (`startup/common.js` `resolveHost`):

| Mode (`EVEN_HOST_MODE`) | Address |
|---|---|
| unset (LAN, the default) | First IPv4 that isn't internal and is in `10/8`, `192.168.0.0/16` or `172.16.0.0/12`, in `os.networkInterfaces()` order. If none is found, it warns and binds `127.0.0.1` only. |
| `tailscale` | First line of `tailscale ip -4`. It exits if that's unavailable. |
| `interface` (`EVEN_HOST_INTERFACE`) | First IPv4 of the named interface. It exits if there isn't one. |
| expose provider set | `""`, so it binds `127.0.0.1` only. The public URL comes from the tunnel. |

- **Listening:** the server listens on `<address>:PORT`. If `<address>` isn't `127.0.0.1`, a
  **second listener** serves the same app on `127.0.0.1:PORT`. It does **not** bind `0.0.0.0`,
  despite the README.
  - Verified live: `192.168.1.85:3456` and `127.0.0.1:3456`, both owned by the same pid.
- The Codex app-server (`8765`) is out of scope.

### 1.3 Persistent config (`~/.even-terminal/config.json`, mode 0600)

```json
{ "version": 1, "provider": "claude", "cwd": "/home/user/work", "network": {"mode": "lan"},
  "port": 3456, "token": "TOKEN", "claude": {"useSystemCli": false} }
```

| Field | Meaning | Env var it maps to |
|---|---|---|
| `version` | Must be `1` | — |
| `provider` | `claude` \| `codex` | `DEFAULT_PROVIDER` |
| `cwd` | Absolute path. The cwd for **new** sessions. | `PROJECT_DIR` |
| `network` | `{mode:"lan"}` \| `{mode:"tailscale"}` \| `{mode:"interface",name}` \| `{mode:"expose",provider}` | `EVEN_HOST_MODE` / `EVEN_HOST_INTERFACE` / `EVEN_TERMINAL_EXPOSE_PROVIDER` |
| `port` | 1–65535 | `PORT` |
| `token` | Pairing token. Stays the same across restarts. Generated as 16 random bytes in hex (32 chars). | `BRIDGE_TOKEN` |
| `name` | Optional display name. Goes into the QR. | `EVEN_TERMINAL_NAME` |
| `claude.useSystemCli` | Use `claude` from `PATH` instead of the SDK-bundled CLI | `EVEN_TERMINAL_CLAUDE_CODE_EXECUTABLE` |
| `claude.allowedTools` | Overrides the list of auto-approved tools | `EVEN_TERMINAL_CLAUDE_ALLOWED_TOOLS` (JSON array) |

- **Precedence:** CLI flag > inherited env var > config file.
- Unknown keys are rejected. If there's no `BRIDGE_TOKEN`, the server generates a random token for that run.
- **Pidfile:** the server also writes `~/.even-terminal/instances/<pid>.json`
  (`{pid,platform,startedAt,port,token,cwd,codexAppServerPort}`, mode 0600). The `even-terminal claude`
  and `even-terminal codex` CLI wrappers use it. The app doesn't.
- **Pairing contract:** the token must stay stable, or the phone has to re-scan. Reusing this config
  file in even-cc keeps the existing pairing.

---

## 2. Auth and CORS

### 2.1 Middleware order (Express)

1. If `EVEN_ALLOW_CORS=1` (`--allow-cors`), `cors()` runs first.
2. The request logger.
3. `express.json({limit:"10mb"})`. This runs **before auth**, so a malformed JSON body to any path
   returns Express's **HTML 400** page (a SyntaxError stack) even without a token.
4. `POST /api/claude-sync/hook`. This is loopback-only and has **no auth**; it's for the terminal
   wrapper and out of scope.
5. `app.use("/api", auth, eventsRouter)`, then `app.use("/api", auth, coreRouter)`.

Everything under `/api` needs auth, including unknown `/api/*` paths and `OPTIONS`. Anything outside
`/api` gets no auth and returns Express's 404 HTML.

The claude-sync WebSocket upgrade on `/api/claude-sync/ws` is loopback-only and out of scope.

### 2.2 Token check

```js
const provided = header?.startsWith("Bearer ") ? header.slice(7) : req.query.token;
if (provided !== TOKEN) → 401
```

- If an `Authorization` header starting with `Bearer ` (case-sensitive) is present, it **wins**, even
  when the query token is correct. Verified: `Bearer wrong` plus a correct `?token=` gets 401.
- Any other header (for example `Basic xyz`) falls back to `?token=`. Verified: 200.
- A repeated `?token=a&token=b` parses to an array, which never matches, so it's a 401.
- The comparison isn't constant-time.
- **401 response:** `Content-Type: application/json; charset=utf-8`

```json
{"error":{"code":"auth_failed","message":"Unauthorized"}}
```

  (`http/error.auth.401.json`). The server logs `[auth] 401 <METHOD> <url with token=REDACTED> (ip=…)`.
  No 401 appears in the phone logs.

### 2.3 CORS

- **Default (no flag):** no `Access-Control-*` headers at all. A preflight `OPTIONS /api/prompt`
  without a token gets **401**. With a token, Express answers `OPTIONS` itself: 200, `Allow: GET, HEAD`,
  body `GET, HEAD`.
- **`--allow-cors`:** `cors()` defaults apply, mounted before auth.
  - Every response gets `Access-Control-Allow-Origin: *`.
  - Preflights get **204** with `Access-Control-Allow-Methods: GET,HEAD,PUT,PATCH,POST,DELETE`, and
    the requested headers are reflected back. No auth is needed. **[inferred from cors@2 defaults]**
- The phone is a native app, so CORS doesn't matter to it. even-cc can skip CORS unless the flag is set.

### 2.4 Common response conventions

- JSON responses come from Express `res.json`: `Content-Type: application/json; charset=utf-8`, a weak
  `ETag`, and `X-Powered-By: Express`. The app is unlikely to depend on the headers. **[inferred]**
- Error bodies on core routes are `{"error":"<string>"}`. The **only** nested-error shapes are the
  401 above and `/api/info`'s 500 (`{"error":{"code":"info_failed","message":…}}`).
- **Provider validation** (core routes only, not `/api/events`): if `req.query.provider` or
  `req.body.provider` is present and isn't one of `claude`, `claude-sync`, `codex`, the response is

```
400 {"error":"Unsupported provider \"<p>\". Supported providers: claude, claude-sync, codex"}
```

  (`http/error.unsupported-provider.400.json`)

- `provider` values in responses: always **`"claude"`** in the default configuration. `claude-sync`
  only appears when the server is run with `--use-original-claude`. even-cc should emit `"claude"`.

---

## 3. HTTP routes

All paths start with `/api` and need auth (§2). The "Fixture" column points into
`testdata/even/http/`.

### 3.1 `GET /api/info`

- Query: `provider` (optional). The phone sends none.
- **200**, fixture `info.200.json` (live):

```json
{"account":{"email":"user@example.com","organization":"Example Org","subscriptionType":"team"},"model":"Opus 5.5","version":"2.1.312","provider":"claude","extra":{"expose":"off"}}
```

- **`version`:** `claude --version` stdout (3 s timeout), trimmed, with `" (Claude Code)"` removed.
  It's `"Unknown"` on failure. This is the `claude` on `PATH`, not the bundled one.
- **`model`:**
  - Source: `message.model` of the **last assistant entry**, searching the 3 most recently modified
    sessions (SDK `listSessions({limit:3})` + `getSessionMessages`).
  - Display formatting: strip `claude-`, strip `/-\d{8,}$/`, then split on `-`. Capitalise part 0 and
    join the rest with `.` after a space: `claude-opus-5-5` → `Opus 5.5`,
    `claude-sonnet-4-5-20250929` → `Sonnet 4.5`.
  - It's `"Unknown"` if no model is found.
- **`account`:** from `claude auth status` (5 s timeout, JSON) →
  `{email: auth.email??"", organization: auth.orgName??"", subscriptionType: auth.subscriptionType??""}`.
  On any failure it's `{}`.
- **`extra.expose`:**

| Value | When |
|---|---|
| `"pinggy"` \| `"bore"` \| `"ngrok"` | That expose provider is configured |
| `"other"` | Some other expose value is set |
| `"tailscale"` | `EVEN_HOST_MODE=tailscale` |
| `"other"` | Any other `EVEN_HOST_MODE` |
| `"off"` | Nothing set |

- **500** if `getInfo` throws: `{"error":{"code":"info_failed","message":"…"}}`. In practice it never
  throws, because every step has its own try/catch.
- **Cost:** 230 ms–5.8 s per call in the logs, and the phone polls it every 10 s. Cache it in even-cc.

### 3.2 `GET /api/update-check` (not called by the phone)

- **200**, fixture `update-check.200.json`:

```json
{"packageName":"@evenrealities/even-terminal","currentVersion":"0.10.5","newestVersion":"0.10.5","updateAvailable":false,"checkedAt":"2026-10-30T07:07:04.223Z"}
```

- **On npm fetch failure** (5 s timeout), still 200:
  `{"packageName","currentVersion","checkedAt","newestVersion":null,"updateAvailable":null,"error":"<msg>"}`,
  in that key order.
- `updateAvailable` compares the three dot-separated numeric parts.

### 3.3 `GET /api/sessions`

- **Query:**
  - `provider`: defaults to `DEFAULT_PROVIDER`.
  - `limit`: `Number(limit) || 10`. **There's no cap**; `0` and non-numbers become 10.
  - `cwd`: optional filter. The phone never sends `limit` or `cwd`.
- **200**, fixture `sessions.claude.200.json` (live, 10 items):

```json
{"sessions":[{"id":"b9f3dc0c-7bdb-4174-bf79-cf29f9a53f30","title":"EVEN","timestamp":"2026-10-30T07:05:24.770Z","cwd":"/home/user/work","provider":"claude","status":"idle"}, …]}
```

**Item fields:**

| Field | Type | Source |
|---|---|---|
| `id` | string | Session UUID (the transcript file name) |
| `title` | string | `(customTitle \|\| summary \|\| firstPrompt \|\| "").slice(0, 64)`. The slice is in **UTF-16 code units**, has no ellipsis, and can split a surrogate pair. |
| `timestamp` | string | `new Date(lastModified).toISOString()`. `lastModified` is the transcript file's **mtime** in ms, giving the form `YYYY-MM-DDTHH:MM:SS.mmmZ`. |
| `cwd` | string | Session cwd, or `""` |
| `provider` | string | `"claude"` |
| `status` | string \| null | For the **first 10 items only**: the in-memory state, `"idle"` \| `"busy"` \| `"awaiting"`. A session this server hasn't run since startup is `"idle"`. Items from index 10 on keep `null`. |

**Session listing semantics.** These come from SDK `listSessions({limit})`, or `({dir: cwd, limit})`
when `cwd` is given. Source is SDK 0.3.314; the minified internals are summarised here.

- **Scope:** every `~/.claude/projects/*/<uuid>.jsonl`, across **all projects** when `cwd` is absent.
  That includes sessions from VS Code, the CLI and the SDK.
  - With `dir`, the listing covers that project plus its git worktrees (default `includeWorktrees: true`).
- Only the first and last **64 KiB** of each file are read (head and tail).
- **Skipped** if the first line has `"isSidechain":true`, or if no title candidate exists at all.
- **Title chain** (SDK `summary`; first non-empty wins):
  1. `customTitle` (last occurrence in the tail)
  2. the custom-title sidecar
  3. `customTitle` in the head
  4. `aiTitle` in the tail
  5. `aiTitle` in the head
  6. `lastPrompt` in the tail
  7. `summary` in the tail
  8. `firstPrompt`
  9. `"Image"` or `"Document"`

  Since `summary` already falls back through this whole chain, the server's
  `customTitle || summary || firstPrompt` equals the SDK `summary` in practice.
- **`firstPrompt`** is the first `type:"user"` line in the head that:
  - isn't a `tool_result`, `isMeta` or `isCompactSummary` entry;
  - isn't text starting with an XML-ish tag (`/^\s*<[a-z][\w-]*[\s>]/`) or `[Request interrupted by user…]`.

  A `<command-name>X</command-name>` gives fallback `X`, and `<bash-input>` gives `! cmd`. Newlines
  become spaces, and the result is trimmed and cut to 200 chars plus `…`.
- **`cwd`** is the last `relocatedCwd` record, else the first `"cwd":"…"` in the head, else the project path.
- **Dedupe and sort:** dedupe by `sessionId`, keeping the newest. Sort by mtime **descending**, with ties
  broken by `sessionId` **descending**. Verified live: three sessions with identical timestamps came
  back ordered `fa1abd9f…`, `dbb39542…`, `5b38c94f…`. Then apply `limit`.

**Errors:**

- Any exception still returns **200** `{"sessions":[],"error":"<message>"}`.
- **`provider=codex`:** the phone polls this every cycle. The live server answers
  `{"sessions":[],"error":"codex app-server failed to start (see [codex] logs above)"}`
  (`sessions.codex-unavailable.200.json`). even-cc should return 200 `{"sessions":[]}`, or that exact
  error variant, and never 400. **[inferred: the app tolerates both; the error variant is what it gets today]**

### 3.4 `GET /api/sessions/:id/history`

- **Query:** `limit` → `Math.min(parseInt(limit) || 10, 10)`, so the **cap is 10** and `0` or a
  missing value becomes 10. `provider` is optional.
  - The phone sends `limit=10&provider=claude`.
  - Quirk: a negative limit `-n` turns into `slice(n)`. Ignore it.
- **200**, fixture `history.200.json` (live, strings cut to 2000 chars):

```json
{"history":[{"role":"user","text":"Okay, well, can you, uh, have a fable 5.1 do a full code review …"},{"role":"assistant","text":"I've started a Fable 5.1 review …"}, …]}
```

- **Derivation:** take SDK `getSessionMessages(id)`. That's the **main chain** from the latest leaf via
  `parentUuid`, user/assistant only. It drops `isSidechain`, team and `isMeta` entries, but
  compact-summary messages stay.
  - If `message.content` is a **string**: if it's non-empty, push `{role: <entry.type>, text: content}`.
    The text is **raw**, so it can include `<task-notification>…`, `<command-name>…` and so on. The
    live fixture's 3rd item is a `<task-notification>` user message.
  - If it's an **array**: push one item per block with `type:"text"`. Empty text is still pushed.
    `tool_use`, `tool_result`, `thinking` and image blocks are skipped. One assistant message can
    yield several items.
  - Return the **last** `min(limit, 10)` items, oldest first.
  - `role` is `"user"` \| `"assistant"`.
- **Unknown, invalid or missing session:** 200 `{"history":[]}` (`history.unknown-session.200.json`).
- **Exception:** 200 `{"history":[],"error":"<msg>"}`.

### 3.5 `GET /api/events`: SSE (§4)

- Without `sessionId`: **400** `{"error":"Missing 'sessionId' query parameter"}`
  (`error.events-missing-sessionId.400.json`). Note the wording differs from the other routes.

### 3.6 `POST /api/prompt`

- **Body** (JSON): `{text: string, sessionId?: string, provider?: string, cwd?: string}`.
  - Per the `[prompt]` log line, the phone sends `sessionId` and `provider:"claude"`.
  - Whether it also sends `cwd`, and in what key order, isn't logged. **[inferred: `{sessionId,text,provider}`]**
- **202**, fixture `prompt.202.json`. The body is reconstructed from code; the status is from the logs.

```json
{"ok":true,"sessionId":"b9f3dc0c-7bdb-4174-bf79-cf29f9a53f30","provider":"claude"}
```

**Behaviour:**

- **Missing or non-string `text`:** **400** `{"error":"Missing 'text' field"}`.
- **`sessionId` given:**
  - If the session isn't in memory, the server looks it up with SDK `getSessionInfo`, falling back to
    scanning `~/.claude/projects/*/<id>.jsonl`.
  - If it's not found: **404** `{"error":"Claude session not found: <id>"}`
    (`error.prompt-session-not-found.404.json`, live).
  - The resume cwd is the session's recorded cwd; the request's `cwd` is **ignored**.
- **`sessionId` absent** (new session): cwd = `body.cwd ?? PROJECT_DIR`. If that path doesn't exist,
  the server falls back to its own cwd.
  - The response waits up to **10 s** for the CLI to report the new session id. If the id hasn't
    arrived by then, the reply is `"sessionId":""`. **[not observed: the phone never created a session]**
- **Session open elsewhere** (even-cc addition): when another live Claude Code process on this machine
  (a terminal, VS Code, the desktop app) holds `sessionId`, per `<claudeDir>/sessions/<pid>.json`,
  the reply is **409** `{"error":"Open in VS Code. Close it there, or fork it here."}`. Two writers
  would interleave or fork the transcript. With `fork: true` the server instead starts a new session
  (`--resume <id> --fork-session --session-id <new>`, in the source's cwd) and the reply names it.
  `/api/sessions` items and the history reply carry `openElsewhere: "<client>"` for such sessions.
- **Session busy:** the prompt is **queued** (FIFO), and the response is still 202. `user_prompt` is
  emitted **immediately** (mid-turn); the queued turn starts after the current `result`.
- Every accepted prompt emits SSE `user_prompt` once the session id is known (§5).
- **Other exceptions:** `err.statusCode` or 500, with `{"error":"<message>"}`.
- **Latency:** the 202 is sent once the turn is *started*, not finished. The logs show 2–10 ms when
  warm, but **40–80 s** on [upstream] cold starts. That delay came from an event-loop stall in the
  SDK, which the [warm] patch fixed. The phone tolerated it. even-cc should reply immediately.

### 3.7 `POST /api/permission-response`

- **Body:** `{sessionId, decision, provider?}`, where `decision` ∈ `"allow"` \| `"allowAlways"` \| `"deny"`.
  A missing decision means `"deny"`. Real body (logged):

```json
{"sessionId":"b9f3dc0c-7bdb-4174-bf79-cf29f9a53f30","decision":"allow","provider":"claude"}
```

- **Missing `sessionId`:** 400 `{"error":"Missing 'sessionId'"}`.
- **Session not in this server's memory** (never prompted since startup): **404** `{"error":"Session not found"}`.
- **Decision not among the offered `options[].key`, or no permission pending:** **400**
  `{"error":"Permission decision was not offered or no permission request is pending"}`.
  - Example: `allowAlways` when it wasn't offered.
- **Otherwise:** 200 `{"ok":true}` (`ok.200.json`). The answer applies to the **oldest** pending
  request (FIFO; subagents can stack several).

### 3.8 `POST /api/question-response`

- **Body:** `{sessionId, answer, provider?}`. A missing or empty answer means `"skip"`. Real body (logged):

```json
{"sessionId":"b9f3dc0c-7bdb-4174-bf79-cf29f9a53f30","answer":"{\"Test from the glasses: which do you pick?\":\"Choice A\"}","provider":"claude"}
```

- Missing `sessionId` → 400. Session not in memory → 404 (same strings as §3.7).
- **Otherwise always 200 `{"ok":true}`**, even when no question is pending. The answer is then dropped.
- Encoding and semantics are in §6.2.

### 3.9 `POST /api/interrupt` (not observed from the phone)

- **Body:** `{sessionId, provider?}`.
- Missing `sessionId` → 400 `{"error":"Missing 'sessionId'"}`. Not in memory → 404
  `{"error":"Session not found"}` (both live).
- **Otherwise 200 `{"ok":true}`**, even when idle.
- The server calls the SDK's `query.interrupt()`. The turn then ends with
  `result{success:false,text:"Interrupted by user"}` followed by `status idle` (§5).

### 3.10 `GET /api/status` (not observed from the phone)

- **Query:** `sessionId` (required), `provider`.
- **200** `{"state":"idle","sessionId":"…","provider":"claude"}` (`status.200.json`).
  - `state` is `idle` \| `busy` \| `awaiting`.
  - `awaiting` means a permission or question is pending.
- Missing `sessionId` → 400 `{"error":"Missing 'sessionId'"}`. **Not in memory → 404**
  `{"error":"Session not found"}`, even if the transcript exists on disk.

### 3.11 `GET /api/messages` (polling alternative to SSE; not observed from the phone)

- **Query:** `sessionId` (required), `after` (`parseInt || 0`), `provider`.
- **200:** `{"messages":[{"id":N, …event fields}], "state", "sessionId", "provider"}`.
  - `messages` = the buffered events with `id > after`, oldest first. Each is `{id, ...event}`, so
    **`id` is the first key**.
  - `state` = in-memory state, or `"idle"`.
  - `provider` = the session's provider, else the query `provider`, else **`null`**.
- The unknown-session fixture is
  `{"messages":[],"state":"idle","sessionId":"nope","provider":null}` (`messages.unknown-session.200.json`).
- A real excerpt is in `messages.excerpt.200.json`: ids 1237–1252 cut from a live `after=0` response,
  covering a turn end and the next turn's start.
- Missing `sessionId` → 400 `{"error":"Missing 'sessionId'"}`.

### 3.12 Debug and misc routes (never called by the phone; optional for even-cc)

| Route | 200 response |
|---|---|
| `GET /api/metrics` | `{"codex":{"subscribedSessions":[]}}` (`metrics.200.json`) |
| `GET /api/debug/status/:id` | `{"sessionId":"…","provider":"claude","status":"idle"}` (`debug-status.200.json`). 500 → `{"error":msg}`. |
| `GET /api/debug/thread/:id` | `{"sessionId":id,"messages":<raw SDK getSessionMessages>}`. 500 → `{"error":msg}`. |
| `POST /api/codex/ensure-app-server` | Codex only. Skip it. |

Unknown `/api/*` with auth: Express HTML **404** `Cannot GET /api/nonexistent`.

---

## 4. SSE stream

### 4.1 Request

`GET /api/events?token=<token>&sessionId=<id>[&needReplay=true]`. The phone sends exactly
`?token=…&sessionId=…`, in that order. Auth is via query or header (§2.2). There's no provider check
and no check that the session exists. Any non-empty `sessionId` creates an empty per-session buffer.

### 4.2 Response framing (byte-exact)

Headers (live):

```
HTTP/1.1 200 OK
X-Powered-By: Express
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
X-Accel-Buffering: no
Transfer-Encoding: chunked
```

The headers are flushed immediately, then:

```
:ok\n\n                                   ← always first (fixture events.connect-wire-sample.txt)
id: <n>\ndata: <JSON.stringify(event)>\n\n ← each event
:heartbeat\n\n                            ← every 15 s per connection (setInterval 15000)
```

- There's one `data:` line per event, compact JSON with no newlines. There's no `event:` field and no
  `retry:` field.
- The space after `id:` and `data:` is present.
- The event JSON on the wire does **not** contain `id`. The SSE `id:` line carries it.
  Contrast `/api/messages`, which merges `id` into the object.
- Fixture `events.replay-wire-sample.txt` holds the exact bytes of `:ok` plus 6 replayed frames.

### 4.3 Ids, buffer and replay

- **Per-session state:** `{messages: [], clients: Set, nextId: 1, activeTurnStartId: null}`, kept in
  memory and lost on restart. Ids start at **1 per session** and increase by 1 for every event of that
  session.
  - even-cc differs here: a session's ids keep increasing across restarts (README "Event ids"),
    because the app appears to ignore ids it has seen when it reconnects to a restarted server.
- **Ring buffer:** 500 events per session (`MAX_MESSAGES_PER_SESSION`). Verified live:
  `/api/messages?after=0` returned exactly 500 entries, ids 1115–1614.
- **`activeTurnStartId`:** set to the id of the first `user_prompt`, or `status busy`, when it's null.
  It's cleared on `status idle`.
  - Trimming keeps everything from `min(id-499, activeTurnStartId)`. So an in-progress turn is never
    trimmed, and the buffer can grow past 500 during a long turn.
- **Replay:** only when `needReplay=true` (the exact string) **and** the buffer is non-empty. The server
  writes **all** buffered events (`id:`/`data:` frames) right after `:ok`, then attaches the client to
  live events.
  - **`Last-Event-ID` is ignored.** There's no `after` param on `/api/events`.
  - The phone never sends `needReplay`. After connecting, it rebuilds context from `/history`. Events
    emitted while no client was connected are therefore lost to the phone, except through history.
- **Fan-out:** every connected client for that `sessionId` gets every event. Several clients per
  session are allowed; the logs show the phone holding 2 sessions' streams at once.
- A failed write removes the client. On the client's `close`, the heartbeat stops and the client is removed.

### 4.4 Sessions without an id yet

The server's `emit(sessionId, msg)` **drops** any event whose sessionId is `""` or undefined: it's not
buffered and not sent.

- For a new session, nothing is emitted until the CLI reports the session id (its first stream message,
  `system/init`).
- `user_prompt` is deferred until the id is ready, and is then emitted **before** `status busy`.
- A `running_stats` tick that fires before the id is known is lost.
- `/api/events` with `sessionId=` empty gets a 400. The phone has to learn the id from the 202 response
  (or `/api/sessions`), then connect.

---

## 5. Event types (Claude)

Key order is as emitted. JSON keys whose value is `undefined` in JS are **omitted**. Every example
below is copied from the logs (`testdata/even/events/<type>.jsonl` holds more).

| type | Fields (in order) | Emitted when |
|---|---|---|
| `user_prompt` | `type`, `text: string` (full prompt) | Each accepted `POST /api/prompt`, when the id is known. For a queued prompt, at POST time. |
| `status` | `type`, `state`, `sessionId: string`, `provider: "claude"` | See the states below |
| `text_delta` | `type`, `text: string` | Each `content_block_delta` / `text_delta` from the main agent |
| `tool_start` | `type`, `name: string`, `toolId: string` | `content_block_start` of a `tool_use` block (main agent only) |
| `tool_end` | `type`, `name`, `toolId`, `summary: string`, `detail: {input: object, output?: string}` | The `tool_result` arrives in a user message. **Also for subagent tools, with no matching `tool_start`.** |
| `permission_request` | `type`, `toolName`, `description`, `detail`, `toolUseId`, `options: [{text,key}]`, `suggestions: array\|null` | The SDK `canUseTool` fires for a tool that isn't auto-allowed (§6.1) |
| `permission_result` | `type`, `toolName`, `summary`, `decision: "allowed"\|"always"\|"denied"` | Answered, timed out or aborted |
| `user_question` | `type`, `questions: [{question, header, multiSelect: bool, options: [{label, description, preview}]}]`, `toolUseId` | `AskUserQuestion` reaches `canUseTool` |
| `question_answer` | `type`, `answers: object` (normally `{questionText: answer}`) | Answered, timed out or aborted |
| `notification` | `type`, `title: string`, `message: string` | The CLI `Notification` hook (`title` defaults to `"Notice"`), or SDK `system/api_retry` |
| `running_stats` | `type`, `durationMs`, `inputTokens`, `outputTokens` (ints) | Every **10 s** while busy |
| `result` | `type`, `success: bool`, `text`, `sessionId`, `costUsd: number`, `provider`, `turns`, `durationMs`, `inputTokens`, `outputTokens` | SDK `result` message |
| `task_progress` **[not observed]** | `type`, `completed: int`, `total: int`, `current: string` | `TodoWrite` (auto-allowed) |
| `error` **[not observed]** | `type`, `message: string` | The query loop threw (not an abort) |

**`status.state` values:**

| State | Emitted when |
|---|---|
| `busy` | First SDK message of each turn |
| `think_start` / `think_end` | `content_block_start` / `content_block_stop` of a `thinking` block. Thinking text is **never** sent. |
| `text_start` / `text_end` | The same, for a `text` block |
| `idle` | The turn is over |

`awaiting` is never sent as an SSE status for Claude; it only appears via `/api/status` and `/api/sessions`.

### 5.1 Field details

- **`tool_end.summary` and `permission_request.description`:** `summarizeClaudeToolCall(name, input)`
  (§5.3).
- **`tool_end.detail.input`:** the raw tool input object, with Claude's key order preserved.
- **`tool_end.detail.output`:** `stringifyToolResponse(tool_result.content)`:
  - a string content passes through unchanged;
  - an object with `stdout`/`stderr` gives `stdout` + `"\n"` + `stderr` (or whichever is non-empty);
  - an array `content` gives its text blocks joined with `"\n"`;
  - anything else is `JSON.stringify`'d.

  Note the top-level array case: `content` that is *itself* an array is an `object` in JS, so it hits
  `JSON.stringify`. The live Agent and ToolSearch samples show the stringified array, for example
  `"[{\"type\":\"tool_reference\",\"tool_name\":\"TaskStop\"}]"`.
  The key is **omitted** when the content is `undefined` or `null`. **Outputs are not truncated** (the
  logs have 30 KB `Read` outputs).
- **`permission_request.detail`:** the first non-empty of `input.command`, `file_path`, `url`, `prompt`,
  `query` (each cut to **200** UTF-16 units, no ellipsis, no whitespace folding), then `input.content`
  (cut to **100**), else `""`.
- **`suggestions`:** the SDK's `PermissionUpdate[]` passed through raw, or `null`. Observed values are
  `[{"type":"setMode","mode":"acceptEdits","destination":"session"},{"type":"addDirectories","directories":["/home/user/.claude"],"destination":"session"}]`
  and `[]`.
- **`options`:**
  - Always starts with `{"text":"Yes","key":"allow"}` and ends with `{"text":"No","key":"deny"}`.
  - `allowAlways` is inserted in between only when the **first** suggestion yields text:

| First suggestion | Text |
|---|---|
| `addDirectories` (non-empty) | `` Yes, and always allow access to `d1`, `d2` <dest> `` |
| `addRules` / `replaceRules` (non-empty) | `` Yes, and always <behavior\|allow> <toolName\|this tool>[ rule `<ruleContent>`] <dest> `` |
| `setMode` | `` Yes, and use `<mode>` permission mode <dest> `` |
| Any other non-empty suggestions | The `description` string |

  - `<dest>`: `session` → `for this session`, `projectSettings` → `for this project`, `localSettings` →
    `in local settings`, `userSettings` → `in user settings`, `cliArg` → `from CLI settings`,
    default → `for this project`.
  - Real example: `` "Yes, and use `acceptEdits` permission mode for this session" ``.
- **`user_question.questions`** are normalised: `question`, `header` and option strings default to `""`,
  `multiSelect` is `=== true`, and `preview` defaults to `""`.
- **`running_stats`:**
  - `durationMs` = now − start of `run()`.
  - `inputTokens` = Σ over the turn's API calls of `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`.
  - `outputTokens` = Σ of the final `output_tokens` of finished messages, plus the current message's
    running `output_tokens`.
  - The first tick can show `0`/`0`. The timer starts at `run()`; it isn't emitted after the session goes idle.
- **`result`:**
  - `costUsd` = `total_cost_usd ?? 0`. It's a JS number, so `0` is printed as `0` and others as
    shortest round-trip, for example `1.4539427999999999`.
  - `turns` = `num_turns`, `durationMs` = `duration_ms`.
  - `inputTokens` / `outputTokens` = Σ over `modelUsage[*].inputTokens` / `.outputTokens`.
  - `sessionId` = `result.session_id ?? this.sessionId ?? ""`.
  - In the [warm] build these numbers are the CLI process's **cumulative** totals and grow across
    prompts: 5.76 → 5.99 → 6.10 USD in the logs.
  - **`text`:**

| Result | `text` |
|---|---|
| `subtype:"success"` | `result.result ?? ""` |
| Interrupt (`error_during_execution` + `terminal_reason:"aborted_streaming"`) | `"Interrupted by user"`, with `success:false` |
| `error_max_turns` | errors joined with `\n`, or `"Reached max turns limit (<n> turns). Try breaking the task into smaller steps."` |
| `error_max_budget_usd` | errors, or `"Session budget exhausted."` |
| Any other error | errors joined with `"\n"` |

  - `maxTurns` is 50.
- **`notification` from `api_retry`:** `{"type":"notification","title":"API Retry","message":"Retrying (<attempt>/<max_retries>)[, HTTP <status>]..."}`
  **[not observed]**.
- **`task_progress`:**
  - `completed` = count of `status=="completed"`, `total` = todos.length.
  - `current` = the `content` (or `activeForm`) of the first `in_progress` todo, else `"All done"` if
    all are completed, else `""`.
  - It isn't emitted for an empty list. Newer CLIs use `TaskCreate`/`TaskUpdate`, which emit nothing,
    so this event is rare.

**Real examples, one per observed type:**

```json
{"type":"user_prompt","text":"Okay, so this should be fast."}
{"type":"status","state":"busy","sessionId":"b9f3dc0c-7bdb-4174-bf79-cf29f9a53f30","provider":"claude"}
{"type":"status","state":"think_start","sessionId":"b9f3dc0c-7bdb-4174-bf79-cf29f9a53f30","provider":"claude"}
{"type":"text_delta","text":"ostly yes. The fl"}
{"type":"tool_start","name":"Bash","toolId":"toolu_01BQBQQfWa2VGe7NctYQM172"}
{"type":"tool_end","name":"ToolSearch","toolId":"toolu_01JeWJoHfaVdbb9zQMQsNzHh","summary":"ToolSearch select:TaskStop","detail":{"input":{"query":"select:TaskStop","max_results":1},"output":"[{\"type\":\"tool_reference\",\"tool_name\":\"TaskStop\"}]"}}
{"type":"permission_request","toolName":"Edit","description":"Edit settings.json ~1 lines","detail":"/home/user/.claude/settings.json","toolUseId":"toolu_01Q2aYp255pwzJEAvHMUfaEf","options":[{"text":"Yes","key":"allow"},{"text":"Yes, and use `acceptEdits` permission mode for this session","key":"allowAlways"},{"text":"No","key":"deny"}],"suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"},{"type":"addDirectories","directories":["/home/user/.claude"],"destination":"session"}]}
{"type":"permission_result","toolName":"Edit","summary":"Edit settings.json ~1 lines","decision":"allowed"}
{"type":"user_question","questions":[{"question":"Test from the glasses: which do you pick?","header":"Glasses test","multiSelect":false,"options":[{"label":"Choice A","description":"Pick A to test the ring.","preview":""},{"label":"Choice B","description":"Pick B to test the ring.","preview":""}]}],"toolUseId":"toolu_01JnPaULwDbF5dsm6Wkh5UZr"}
{"type":"question_answer","answers":{"Test from the glasses: which do you pick?":"Choice A"}}
{"type":"notification","title":"Notice","message":"Claude needs your permission to use Edit"}
{"type":"running_stats","durationMs":70048,"inputTokens":115755,"outputTokens":0}
{"type":"result","success":true,"text":"OK2","sessionId":"12fdf5a6-35d6-41e0-a5b1-3e7a60a84191","costUsd":5.976796200000001,"provider":"claude","turns":1,"durationMs":1166,"inputTokens":128898,"outputTokens":71714}
{"type":"status","state":"idle","sessionId":"b9f3dc0c-7bdb-4174-bf79-cf29f9a53f30","provider":"claude"}
```

### 5.2 Ordering within a turn

The order below is observed in 22 turns; the `turn-*.jsonl` fixtures hold three complete ones.

```
POST /api/prompt
 → user_prompt                         (immediately for a known session)
 → [running_stats]                     ([upstream] can come before busy on slow starts)
 → status busy                         (first SDK message of the turn)
 → repeat per content block of the main agent:
     status think_start … status think_end       (no thinking text)
     status text_start, text_delta×N, status text_end
     tool_start                        (tool_use block start; no status pair for tool blocks)
       [permission_request | user_question]      (canUseTool; after tool_start, possibly after several parallel tool_starts)
       [notification "Claude needs your permission to use X"]  (CLI hook, ~0–6 s later, may come after the answer)
       [permission_result | question_answer]
     tool_end                          (when the tool_result arrives; parallel tools can interleave)
     running_stats every 10 s throughout
 → result
 → status idle
```

- **Idle timing:** [warm] sends idle immediately after `result`. [upstream] sends it when the CLI
  process exits, usually within moments.
- **Queued prompt:** if one is waiting, **no idle** is sent. The next turn starts with `status busy`;
  its `user_prompt` was already sent at POST time.
- **A query error:** `error{message}`, then `status idle`.
- **Interrupt:** `result{success:false,"Interrupted by user"}`, then `status idle`.

**Quirks the app must tolerate (all seen in logs):**

1. **A stray empty `result` at the start of a turn.** It came from a leftover background-task
   notification:
   `{"type":"result","success":true,"text":"","sessionId":"3c19…","costUsd":0,"provider":"claude","turns":0,"durationMs":237,"inputTokens":0,"outputTokens":0}`.
   It arrived right after `busy`, and the real turn followed with a second `result`.
2. **`tool_end` with no `tool_start`.** These come from subagent (`Agent`/Task) tool calls, and keep
   arriving **after `status idle`** while background agents run. In the logs, 191 `tool_end` events
   but only 111 `tool_start`.
3. **[warm] Background-agent completions start an extra turn** with no `user_prompt` and no `busy`:
   `think_start … text_delta … result, status idle`.
4. **`running_stats` before `busy`** ([upstream] cold start).

### 5.3 `summarizeClaudeToolCall` (exact strings)

`t(x,n)` = collapse whitespace runs to one space, trim, and if longer than n UTF-16 units, cut to n and
append `...`. `fileName(p)` = last path segment split on `/` or `\`, or `"file"` if `p` is empty.

| Tool | Summary |
|---|---|
| Bash | `Bash ` + t(description ?? command ?? "command", 50) |
| Read | `Read <file>` + ` (lines <offset>-<offset+limit>)` if both are set, else ` (<limit> lines)` if only limit |
| Edit | `Edit <file> +N lines` / `-N lines` / `~<added> lines`, where N = lines(new_string) − lines(old_string), and lines(s) = s.split("\n").length, or 0 if empty |
| Write | `Write <file> (<lines(content)> lines)` |
| Glob | `Glob ` + t(pattern, 40) |
| Grep | `Grep "` + t(pattern, 25) + `"` |
| Agent | `Agent ` + t(description ?? "", 40) |
| Skill | `Skill ` + t(skill ?? name ?? "", 40), trimmed |
| TodoWrite | `TodoWrite update tasks` |
| WebSearch | `Search "` + t(query, 30) + `"` |
| WebFetch | `Fetch ` + t(url, 40) |
| ToolSearch | `ToolSearch ` + t(query, 40) |
| KillShell | `Kill process <pid>`, trimmed |
| Config | `Config <action> ` + t(key, 30), trimmed |
| Mcp | `MCP <server_name>.` + t(tool_name, 30) |
| RemoteTrigger | `Trigger <action ?? "manage">` |
| NotebookEdit | `NotebookEdit <command ?? "edit">` |
| ExitPlanMode, ListMcpResources, AskUserQuestion | The tool name itself |
| ReadMcpResource | `ReadMcpResource ` + t(uri, 40) |
| TaskOutput | `Agent ` + t(task_id, 30), trimmed |
| TaskCreate | `Agent ` + t(subject ?? description ?? "", 40) |
| TaskUpdate | `Agent ` + t(subject ?? "", 40) + ` <status>`, trimmed |
| TaskGet / TaskList / TaskStop | `Agent get` / `Agent list` / `Agent stop` |
| Anything else (MCP tools etc.) | The name (`Task*` → `Agent`), plus ` ` + t(subject \|\| description \|\| content \|\| taskId \|\| action, 40) if present |

**Notes on the table:**

- JS `??` passes `""` through, but `||` doesn't. Bash uses `??`, so an empty `description` produces `"Bash "`.
- In t(), `null`/`undefined` → `""`; numbers go through `String()`.
- Real examples: `Bash Check whether the page changed since my last commi...`,
  `Grep "\.claude.*projects|listSe..."`, `Read settings.json (lines 14-24)`, `Edit session.js +59 lines`,
  `Write drive.mjs (31 lines)`.

---

## 6. Permission and question flows

### 6.1 Permissions

**Which tools ask** (SDK `permissionMode:"default"`, with `canUseTool` handling everything that isn't
pre-allowed):

- **Auto-allowed with no event:** `Read, Glob, Grep, ToolSearch, WebSearch, WebFetch, TaskOutput,
  ExitPlanMode, ListMcpResources, ReadMcpResource`, passed as SDK `allowedTools`.
- **Auto-allowed in the callback:**
  - `TodoWrite`, which emits `task_progress`;
  - `TaskUpdate`;
  - `Bash` whose command matches
    `^\s*(ls|cat|head|tail|wc|pwd|echo|printf|date|whoami|which|where|type|file|stat|du|df|printenv|uname|hostname|id|git\s+(status|log|diff|show|rev-parse))\b`
    **and** contains none of `[;&|<>\`\r\n]` or `$(`.
- **Overridable:** `EVEN_TERMINAL_CLAUDE_ALLOWED_TOOLS` (config `claude.allowedTools`) replaces the
  whole default list.
- Project and user `settings.json` permission rules also apply (`settingSources: ["user","project"]`),
  so tools allowed there never reach the phone.
- **Everything else** → `permission_request`.

**Flow:**

1. **Request.** The server emits `permission_request` and adds the request to a FIFO. Session status
   becomes `awaiting`.
2. **Answer.** The app posts `POST /api/permission-response {sessionId, decision}`. The decision must
   be one of the `options[].key` values offered, or the server returns 400 and the request stays
   pending. `allow` → allow. `allowAlways` → allow, and the SDK `suggestions` are applied as
   `updatedPermissions`. `deny` → deny with message `"Denied by user"`.
3. **Timeout.** After **60 s** with no answer, or if the SDK aborts, the request is **denied**.
4. **Result.** The server emits `permission_result` with `decision`: `"allowed"` | `"always"` | `"denied"`.
   Then comes the `tool_end`; for a denied tool, the tool_result is Claude's rejection text.

Observed: `allow` on 23:19:16 and 23:28:15. `allowAlways` and `deny` were never observed.

### 6.2 Questions (`AskUserQuestion`)

1. **Request.** The server emits `user_question{questions,toolUseId}`. The question joins the FIFO, and
   status becomes `awaiting`.
2. **Answer.** The app posts `POST /api/question-response {sessionId, answer}`. **`answer` is a string.**

   | `answer` | Parsed as | Resulting `answers` |
   |---|---|---|
   | JSON object text, keyed by question text | `JSON.parse` succeeds | The parsed **value as-is**. Seen: `"{\"Test from the glasses: which do you pick?\":\"Choice A\"}"`. |
   | Any other valid JSON | `JSON.parse` succeeds | The parsed value as-is, even a non-object. `"1"` → `answers: 1`. Edge case. |
   | Not JSON (for example `Choice A`, `skip`) | Raw string | `{ [q.question \|\| q.header \|\| ""]: <raw> }` for **every** question |
   | Missing or empty | `"skip"` | As in the row above |

   - **Multi-select:** the value is the chosen labels joined with commas (`"A, B"`), inside the JSON
     object. **[inferred]** This comes from the same app's encoding as parsed by the claude-sync
     wrapper (`computeSelections`: JSON object keyed by question text or header; a multi-select
     value is `split(",")` then trimmed). It wasn't seen in the logs.
   - **"skip"** means the user dismissed the question. Claude receives the literal answer `"skip"`.
3. **Timeout.** After **120 s**, or on abort, the answer is `"skip"`.
4. **Result.** The server emits `question_answer{answers}`. It returns
   `{behavior:"allow", updatedInput:{questions: <original input.questions>, answers}}` to the SDK.
   The tool_result text is, for example,
   `Your questions have been answered: "<q>"="Choice A". You can now continue with these answers in mind.`

---

## 7. Session semantics summary

- **In-memory session:** created only by `POST /api/prompt` (new or resumed). Until that happens,
  `status`, `interrupt`, `permission-response` and `question-response` return **404**, and
  `/api/sessions` reports it as `"idle"`.
- **States:** `idle` → `busy` (on `run()`) → `awaiting` (while any permission or question is pending)
  → `idle` (after result: [warm] immediately; [upstream] on process exit).
- **[warm] process lifetime:** one CLI process per session stays alive between prompts. It closes after
  10 min idle (`EVEN_TERMINAL_WARM_IDLE_MS`), and it's restarted if the transcript was written
  elsewhere or the last turn failed. This is invisible on the wire, apart from §5.2 quirk 3 and the
  cumulative `result` totals.

---

## 8. Other things the app may depend on

- **Version strings:**
  - The server package is 0.10.5 (`update-check.currentVersion`, banner `Even Terminal v0.10.5`).
  - `/api/info.version` is the Claude CLI version (`"2.1.312"`), not the server's.
- **`provider`:** `"claude"` everywhere: status events, result, `/api/info`, the session list, the
  prompt reply and `/api/status`. `/api/messages` falls back to `null` for unknown sessions.
- **Timestamps** in `/api/sessions` are ISO-8601 UTC with milliseconds.
- **JSON encoding parity for Go:**
  - Use `json.Encoder.SetEscapeHTML(false)`. JS doesn't escape `<`, `>` or `&`.
  - JS doesn't escape U+2028/U+2029; Go does, so post-process those or write a custom encoder.
  - Keep struct field order exactly as in §5.
  - Pass tool `input`, `suggestions` and `answers` through as `json.RawMessage`. A `map[string]any`
    would re-sort the keys.
  - Omit `detail.output` when it's nil. Floats: Go's `float64` marshal matches JS shortest round-trip
    (`0` stays `0`).
- **String lengths** (64-char title, 50/40/30/25 summaries, 200/100 detail) count **UTF-16 code units**.
- **Responsiveness:** the app polls `/api/info` and `/api/sessions` every 10 s. In the logs it dropped
  its SSE connection right after an 80 s `/api/prompt`. Keep handlers fast, and never block SSE writes.

---

## 9. Fixtures (`testdata/even/`)

Every file is real data unless marked otherwise.

- Strings longer than 2000 chars are cut to 2000 and end with `…[truncated N chars]`.
- The bearer token is replaced with `TOKEN`, and `*_TOKEN=` / `*SECRET*=` style env values with `REDACTED`.
- Samples were scanned for secret-like content.

| File | Source |
|---|---|
| `events/<type>.jsonl` (status, text_delta, tool_start, tool_end, permission_request, permission_result, user_question, question_answer, notification, running_stats, result, user_prompt) | `[SSE-…]` lines in the phone logs. One JSON object per line, exactly as on the wire. `status.jsonl` has 2 per state. `result.jsonl` includes the empty `turns:0` quirk. |
| `turn-simple-bash.jsonl` | Phone turn 2026-10-29 23:22:30 [warm]: prompt → busy → Bash → text → result → idle |
| `turn-ask-user-question.jsonl` | Phone turn 22:44:12 [upstream]: AskUserQuestion, a question-response, notification, question_answer |
| `turn-permission-edit.jsonl` | Phone turn 23:18:21: parallel Edit and Bash, permission_request, permission-response `allow`, permission_result |
| `http/*.json`, `*.html`, `*.txt` | Live captures from the running server, 2026-10-30. **Except:** `prompt.202.json`, `ok.200.json` and `error.permission-not-pending.400.json` are built from code (their status codes are confirmed by logs), and `messages.excerpt.200.json` is a slice (ids 1237–1252) of a live `after=0` response. |

**Turn files** use one JSON object per line:

- `{"ts","kind":"sse","sessionId","data":<event>}`
- `{"ts","kind":"http","method","path","request"}`
- `{"ts","kind":"http-response","method","path","status","durationMs","response","reconstructed":true}`

**Not real:**

- The `/api/prompt` request body is **reconstructed**: the full text comes from the `user_prompt`
  event, and the fields from the `[prompt]` log line. The key order is unknown.
- HTTP response bodies are reconstructed from code (`"reconstructed":true`).
- The permission-response and question-response **request** bodies are exact copies of logged bodies.
- SSE `id:` values aren't in the logs, so they're omitted.

**No fixture** exists for `task_progress`, `error`, `/api/interrupt` success, or the interrupt
`result`. None of these were observed.

---

## 10. Open questions

1. Does the app ever send `needReplay=true`, `Last-Event-ID`, or call `/api/messages`, `/api/status` or
   `/api/interrupt`? None appear in ~2 days of logs. Interrupt presumably sits behind the "Stop" control
   and needs a deliberate test.
2. What's the exact `/api/prompt` body (is `cwd` included, and in what key order), and does the app ever
   create a new session (prompt without `sessionId`)? How does it handle `sessionId:""`?
3. Multi-select and multi-question answer encoding (comma-joined labels inside the JSON object) is
   inferred from the claude-sync parser and should be confirmed with a live multi-select question.
4. Does the app render `allowAlways` and `deny`, and does it show `task_progress`? Never observed.
5. What does the app do with `GET /?token=…` returning something other than 404, and does it need
   `provider=codex` to return `error` or just an empty list?
