# even-cc

Claude Code on Even Realities G2 glasses, through [Faceclaw](https://github.com/jimrandomh/faceclaw).
even-cc is a single binary that runs next to your Claude Code on a machine and serves it to the **Claude
Code app** in our Faceclaw build (the default branch of [sorbet-ai/faceclaw](https://github.com/sorbet-ai/faceclaw)):

- your sessions and their live events, prompts, permission and question answers
  (see `docs/even-protocol.md` for the HTTP API, `docs/claude-protocol.md` for how it drives `claude`);
- speech-to-text through a local Whisper server, so voice works anywhere in Faceclaw;
- the real Claude Code screen for Faceclaw's Terminal app (Windows).

Only `claude` itself (logged in) is needed at runtime. The Even app / Glassly / Even Hub versions are
on the `even-app` branch.

## Setup

1. **Faceclaw.** Install our Faceclaw build on the Android phone (`../faceclaw`, see its README for
   building; it replaces the official app, so export settings first with `scripts/pull_config.sh`).
   Pair the glasses in it.
2. **even-cc.** Install it on the machine that runs Claude Code (macOS, Linux or Windows; Claude
   Code must be installed and logged in):

   ```sh
   curl -fsSL https://raw.githubusercontent.com/sorbet-ai/even-cc/main/install.sh | sh   # macOS, Linux, WSL
   ```

   ```powershell
   irm https://raw.githubusercontent.com/sorbet-ai/even-cc/main/install.ps1 | iex      # Windows
   ```

   Or with Go 1.27+: `go install github.com/sorbet-ai/even-cc@latest`.

   Then `even-cc start` runs it in the background and prints the setup QR code; `even-cc stop` stops
   it and `even-cc status` says whether it runs. `even-cc` alone runs it in the terminal instead. The
   first run writes `~/.even-cc/config.json` with a random token.
3. **Add the machine.** even-cc prints a QR code. On the glasses, in Claude Code: tap > Settings > Machines >
   **Add machine**, and scan it with the phone. That adds the machine, points Faceclaw's voice at its Whisper
   proxy (if `sttUrl` is set) and adds its terminal mirror to the Terminal app. `even-cc pair` prints
   the QR code again.

   The QR is `http://<pc>:<port>/setup?token=…&name=…[&stt=1][&terminal=g2mirror://…]`. Opening it in
   the phone's browser shows a page that hands off to Faceclaw, but phone browsers often refuse plain
   http pages on the home network, so scanning inside Faceclaw is the reliable way.

## Using it on the glasses

- **Sessions:** the app opens on your machines (or straight on the sessions, with one); pick a machine, then
  a session or **+ New session** (started on that machine).
- **Hold** (ring or watch face): talk; the words show live in the input line. Release, then **tap** to
  send or **double tap** to drop.
- **Slide / crown:** scroll (Settings > Scroll reverses it). Text streams in steps of a third of a
  screen to keep the Bluetooth link free for input.
- **Permission prompts and questions** float over the conversation: slide to pick, tap to answer,
  double tap to hide (tap > Answer prompt brings it back). A question can be answered by voice.
- **Tap:** menu (Answer prompt, Go to latest, Interrupt, Sessions, Settings, Close). **Tap then
  hold:** Faceclaw's system menu.

## Several machines (Windows and WSL)

Run an even-cc on each and add each one. The name defaults to the host name, with ` (WSL)` under WSL.
Two instances on one machine need different ports: a configured port that is taken moves to the next
free one, so set it explicitly for the second (`even-cc config port 3457`). `claudeDir` points an
instance at another Claude config dir (another account).

**WSL with mirrored networking** is reachable from the phone at the Windows address, but WSL's
Hyper-V firewall blocks inbound connections by default. Allow the port once (admin PowerShell):

```powershell
New-NetFirewallHyperVRule -Name even-cc-wsl -DisplayName "even-cc (WSL)" -Direction Inbound `
  -VMCreatorId '{40E0AC32-46A5-438A-A0B2-2B479E8F2E90}' -Protocol TCP -LocalPorts 3457 -Action Allow
```

## Voice: local Whisper

Faceclaw's voice (Claude Code, the assistant, voice input everywhere) can use a Whisper server. A GPU
transcribes in about 0.2 s, and our Faceclaw build refreshes the live text every 0.4 s.

```sh
docker run --detach --restart unless-stopped --publish 8000:8000 --name speaches \
  --volume hf-hub-cache:/home/ubuntu/.cache/huggingface/hub --gpus=all ghcr.io/speaches-ai/speaches:latest-cuda
curl -X POST http://127.0.0.1:8000/v1/models/deepdml/faster-whisper-large-v3-turbo-ct2
even-cc config sttUrl http://127.0.0.1:8000
```

Docker on Windows publishes only to the machine itself, so even-cc forwards `/api/stt/*` to it
(token-protected) and the phone uses `http://<pc>:<port>/api/stt/v1` with the token as its key. Adding
the machine sets that up in Faceclaw.

## Terminal mirror (Windows)

even-cc also speaks the [g2mirror](https://github.com/jimrandomh/g2mirror) protocol on
`mirror.port` (8737): Faceclaw's Terminal app lists its sessions and launches `claude` (preset
`shell` or `claude`; `continue` runs `claude --continue`) in a pseudoconsole, showing the real Claude
Code screen. Spoken key words (`down`, `enter`, `escape`, `shift tab`, digits) are pressed as keys.
`mirror.port 0` turns it off. On Linux use g2mirror itself.

## Config

`even-cc config` prints the config (token masked), `even-cc config <key>` one key, and
`even-cc config <key> <value>` sets one; `--config <path>` for another file. `even-cc serve --help`
lists the flags that override it for one run (`--port`, `--bind`, `--cwd`, `--name`, `--claude`,
`--claude-dir`, `--log-file`, `--log-level`, `--no-qr`).

| Key | Default | Meaning |
|---|---|---|
| `port` | `3456` | HTTP port |
| `token` | random | Pairing token (32 hex chars); also the Whisper proxy and terminal mirror key |
| `bind` | `lan` | `lan` (first private IPv4) · `tailscale` · `interface:<name>` · `loopback`; non-loopback modes also listen on `127.0.0.1` |
| `name` | host name | The machine name in Faceclaw |
| `cwd` | home dir | Working directory for new sessions |
| `claudeDir` | `""` | Claude Code config dir (`CLAUDE_CONFIG_DIR`); empty = `$CLAUDE_CONFIG_DIR` or `~/.claude` |
| `sttUrl` | `""` | Local OpenAI-compatible Whisper server to forward `/api/stt/*` to; empty = off |
| `mirror.port` | `8737` | Terminal mirror port; `0` = off |
| `mirror.args` | `[]` | Extra arguments for the mirrored `claude` |
| `claude.path` | `""` | `claude` / `claude.exe` to use; empty = search `PATH` |
| `claude.model` | `""` | `--model` for launched processes |
| `claude.permissionMode` | `""` | `--permission-mode`; `""` = your Claude settings' default |
| `claude.settingSources` | `user,project,local` | `--setting-sources` |
| `claude.allowedTools` | built-in list | Auto-approved tools (JSON array) |
| `claude.maxTurns` | `0` | `--max-turns` per prompt; 0 = no limit |
| `claude.idleTimeoutMinutes` | `0` | Close an idle warm process after N minutes; 0 = never |
| `claude.extraArgs` | `[]` | Extra CLI arguments |
| `claude.glassesPrompt` | `true` | Append the glasses writing rules (`internal/claude/glasses.md`) to claude's system prompt, so replies are short and screen-sized |
| `timeouts.permissionMinutes` | `10` | Unanswered permission prompt → deny |
| `timeouts.questionMinutes` | `10` | Unanswered question → skip |
| `display.*` | | Tool summaries, subagent lines, reply footer and other event formatting (see `internal/config/config.go`) |
| `log.level` | `info` | `info` · `debug` (adds all CLI traffic, truncated) · `warn` / `error` |
| `log.file` | `""` | Log file; empty = `even-cc.log` next to the config |

## Prompts while Claude waits

While Claude waits for an answer to a question or permission prompt, the CLI is blocked inside that
request, so a prompt sent then answers it instead: as the free-text answer to a question, or as a
denial carrying your words for a permission. A stop word (`stop`, `cancel`, `/ecc stop`) interrupts
the turn. Requests from subagents don't block, so prompts go to Claude as usual then.

## `/ecc` commands

Typed (or spoken) as a prompt; handled by even-cc, never sent to Claude; the reply arrives as a notice.

| Command | Effect |
|---|---|
| `/ecc mode default\|auto\|plan\|acceptEdits` | Permission mode for this session |
| `/ecc model <name>` / `/ecc model default` | Model for this session |
| `/ecc tools full\|collapsed\|hidden` | Tool display, until restart |
| `/ecc thinking on\|off` | Thinking indicator, until restart |
| `/ecc agents on\|off` | Subagent progress lines, until restart |
| `/ecc footer on\|off` | Reply footer, until restart |
| `/ecc stop` | Interrupt the running turn |
| `/ecc status` | State, mode, model, process, background tasks |

## Logs and health

even-cc writes `even-cc.log` next to the config (capped at 5 MB, two old files kept): prompts, turn
results, questions, permission answers, process starts and exits, failed requests. Every session line
carries its id. `--log-level debug` adds every CLI line. `GET /api/ecc/health` (token required) shows
version, uptime, clients and per-session state.

Processes: one warm `claude` per session, reused across prompts and restarted when the transcript was
written elsewhere since. claude runs in its own process group (Unix) or Job Object (Windows), so
stopping it stops what it started; on Windows, even-cc dying takes claude down with it. A panic in a
handler is logged and the server keeps serving; an unrecoverable crash goes to `even-cc.crash.log`.

## Development

Build from source with `go build .` (no C toolchain needed; `GOOS`/`GOARCH` cross-compile).
Pushing a `v*` tag builds every platform and publishes a GitHub release
(`.github/workflows/release.yml`), which the install scripts download.

```sh
go vet ./... && go test ./...
go run ./tools/mirror-e2e ws://127.0.0.1:8738 <token> "<prompt>" "<expected>"   # against a test even-cc
```

Unit tests replay recorded CLI runs (`testdata/claude/`) through the session state machine and check
the HTTP API against recordings (`testdata/even/`). The Faceclaw app's tests live in
`../faceclaw/tests/claude-code.test.cjs`.
