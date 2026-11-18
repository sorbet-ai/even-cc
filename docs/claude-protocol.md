# Driving `claude` (Claude Code CLI) over stdin/stdout: stream-json protocol

Verified 2026-10-30 against **Claude Code 2.1.312** (`/home/user/.nvm/versions/node/v24.22.1/bin/claude`,
which resolves to the native ELF `.../@anthropic-ai/claude-code/bin/claude.exe`). SDK reference read:
`@anthropic-ai/claude-agent-sdk` **0.3.314** (`package.json` says 0.3.314, not 0.3.312) in
`/home/user/work/projects/even-terminal-warm/node_modules/@anthropic-ai/claude-agent-sdk/`.

Everything here was observed in real runs unless marked **[inferred]**. Inferred items come from the SDK source,
from `sdk.d.ts`, or from reasoning about what was observed. Raw transcripts are in `testdata/claude/*.jsonl`
(see [Fixtures](#fixtures)). All example JSON below is copied from those runs. Long arrays are shortened with `…`.

---

## 1. Launch

### 1.1 argv

The SDK always builds this core (from `ProcessTransport.initialize()` in `sdk.mjs`):

```
claude --output-format stream-json --verbose --input-format stream-json [options…]
```

| Flag | Required | Notes |
|---|---|---|
| `--output-format stream-json` | yes | NDJSON on stdout. |
| `--verbose` | **yes** | Without it the process prints `Error: When using --print, --output-format=stream-json requires --verbose` to stderr and exits with code 1 (fixture `noverbose`). |
| `--input-format stream-json` | yes | NDJSON on stdin; this keeps the process alive across turns. |
| `--include-partial-messages` | no | Enables `stream_event` frames (token deltas). Without it you only get whole `assistant` messages. |
| `--permission-prompt-tool=stdio` | for interactive approvals | Routes permission asks to you as `can_use_tool` control requests. The SDK adds it when `canUseTool` is set. **It also enables the `AskUserQuestion`, `EnterPlanMode` and `ExitPlanMode` tools.** Without it they are absent from `system/init.tools` (compare fixtures `minimal` and `multiturn`). |
| `--permission-mode=<m>` | no | `default` \| `acceptEdits` \| `bypassPermissions` \| `plan` \| `dontAsk` \| `auto`. `bypassPermissions` also needs `--allow-dangerously-skip-permissions` **[inferred from SDK]**. |
| `--setting-sources=user,project[,local]` | no | Which settings files load. **Caveat:** allow rules in `~/.claude/settings.json` (here: `Bash`, `Read`, `Write`, `Edit`, …) auto-approve those tools, so **no `can_use_tool` is sent** for them. The probes used `--setting-sources=project` for this reason. It also hides user MCP servers (`mcp_servers: []`). |
| `--model <alias\|id>` | no | E.g. `haiku`, `sonnet`, `opus`, or a full id. The probes used `haiku`. |
| `--max-turns=N`, `--max-budget-usd=X` | no | They end a turn with `result.subtype` `error_max_turns` / `error_max_budget_usd` **[inferred from sdk.d.ts]**. |
| `--resume=<session_id>` | no | Resumes a persisted session. **Not exercised** here: the brief required new sessions only. **[inferred]** |
| `--session-id <uuid>`, `--fork-session`, `--no-session-persistence` | no | **[inferred from SDK argv builder]** |
| `--allowedTools=A,B`, `--disallowedTools=…`, `--tools=…`, `--mcp-config <json>`, `--add-dir <d>`, `--effort <l>`, `--thinking=adaptive\|disabled`, `--max-thinking-tokens N`, `--fallback-model`, `--include-hook-events`, `--debug-file <p>` | no | Passed through by the SDK **[inferred]**. |

You do **not** pass `-p`/`--print`. With piped stdio the CLI runs in print mode on its own; the `--verbose` error
message mentions `--print` even though it was never given.

Things the SDK sends over stdin in `initialize` rather than argv: `systemPrompt` (as `string[]`),
`appendSystemPrompt`, `agents`, `jsonSchema`, `hooks`, `sdkMcpServers`, `title`, `skills`, and so on. See §4.

### 1.2 Environment

`env -i HOME PATH USER LANG` is enough. Auth comes from `~/.claude` credentials (`apiKeySource: "none"` in init means
OAuth/subscription). Fixture `minimal` ran with exactly that env.

What the SDK changes on top of `process.env` (`sdk.mjs` `query()` + `ProcessTransport.initialize()`):

| Var | SDK behaviour | Effect |
|---|---|---|
| `CLAUDE_CODE_ENTRYPOINT` | set to `sdk-ts` if unset | Entrypoint tag for telemetry/feature gates. A Go host can use `sdk-ts` to look identical, or its own value **[inferred: no functional difference observed]**. |
| `CLAUDE_AGENT_SDK_VERSION` | `0.3.314` if unset | Telemetry **[inferred]**. |
| `CLAUDE_CODE_SDK_READS_SESSION_STATE` | `1` unless set | **Observed:** enables `system/session_state_changed` frames (`running`/`requires_action`/`idle`). Without it (fixture `minimal`) they are absent. Recommended. |
| `CLAUDE_CODE_SDK_READS_STARTUP_MARKS` | `1` in some modes | `keep_alive` startup marks **[inferred]**. |
| `NODE_OPTIONS` | **deleted** | |
| `DEBUG` | deleted unless `DEBUG_CLAUDE_AGENT_SDK` | |
| `CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING` | `true` if file checkpointing is on | **[inferred]** |
| `CLAUDE_CONFIG_DIR` | set if the `configDir` option is given | Alternate `~/.claude`. |

**Scrub inherited Claude vars.** If the Go host itself runs under Claude Code, its env contains `CLAUDECODE=1`,
`CLAUDE_CODE_SESSION_ID`, `CLAUDE_CODE_CHILD_SESSION`, `CLAUDE_PID`, `CLAUDE_CODE_MESSAGING_SOCKET/TOKEN`,
`CLAUDE_CODE_ENTRYPOINT`, `CLAUDE_EFFORT`, `AI_AGENT`, and so on. The probes started from an empty env to avoid
inheriting them. What they change in a child is **[inferred]**, but it is safest to drop them.

### 1.3 Process

- `cwd` is the project directory. Sessions persist to `~/.claude/projects/<cwd with / and . turned into ->/<session_id>.jsonl`.
  Example: `/home/user/.claude/projects/-home-user-even-cc--test-cli-probe-work/<id>.jsonl` (see `transcript_path` in hook inputs).
- stdio: all three piped. **stdout = protocol only.** stderr = human diagnostics. Keep the last ~2 KB for error
  messages, as the SDK does (`stderrTail`).
- Start-up time is about 0.5–1.2 s until the `initialize` response.

---

## 2. Framing

- Both directions use **NDJSON**: one JSON object per `\n`-terminated line, UTF-8. Skip blank lines.
- **A malformed stdin line is fatal.** The CLI writes
  `Error parsing streaming input line (type=unknown, 9 chars): SyntaxError` to stderr and **exits with code 1**
  (fixture `badjson`). Serialise each message completely and write it with one `Write`, guarded by a mutex.
- **Key order is not stable.** `result` objects begin with `"duration_api_ms"`, not `"type"`. Decode in two stages
  (`{"type","subtype"}` first, then the concrete struct), or use `json.RawMessage`.
- Every frame carries `type`. Most carry `session_id` and `uuid`. **Ignore unknown `type`s and `subtype`s.** New ones
  appear often (the SDK itself handles `keep_alive`, `transcript_mirror`, `active_goal`, `autocompact_state`,
  `system/post_turn_summary`, `system/task_summary`, `system/session_metadata`, `system/commands_changed`,
  `system/hook_progress`, `system/control_request_progress`, …) **[inferred from sdk.mjs]**.
- Non-JSON stdout lines: the SDK logs and skips them. Do the same.

---

## 3. Message catalogue

### 3.1 Host → CLI (stdin)

| `type` | Purpose |
|---|---|
| `user` | A user turn (prompt). |
| `control_request` | Host-originated request (`initialize`, `interrupt`, `set_permission_mode`, `set_model`, …). |
| `control_response` | Answer to a CLI-originated `control_request` (`can_use_tool`, `hook_callback`, `mcp_message`, …). |
| `control_cancel_request` | Withdraw one of *your own* in-flight requests. No reply **[inferred from sdk.d.ts; not exercised]**. |

**User message** (exact form used in all runs):

```json
{"type":"user","message":{"role":"user","content":"Reply with exactly: OK"},"parent_tool_use_id":null}
```

`message.content` may also be an array of Anthropic content blocks (`text`, `image`, `document`, …). Optional fields
from `sdk.d.ts` **[inferred]**: `session_id`, `uuid` (client id, echoed in some lifecycle frames), `priority`
(`now|next|later`), `shouldQuery:false` (append without starting a turn), `client_composed:true` (no `@file` / slash
expansion).

Sending a `user` message while a turn is running is fine (probe `midturn`, 2.1.312, `.test/cli-probe/probe.mjs`):

- **During a tool call:** the message is folded into the running turn. "Run `sleep 8` … reply A" plus "Also reply B"
  sent 2.4 s in gave **one** `result` `"A\nB"` (`num_turns` 2, `queued_turn_count` 0), then `idle`.
- **After a `result` while a background agent runs:** the CLI sends **no `idle`** after that result (it stays
  `running` until the agent ends), and keeps forwarding the subagent's frames with `parent_tool_use_id` set. A message
  sent then was answered at once: `result` `"C"` ~1 s later, still no `idle`.

So a host must not wait for `idle` before sending the next prompt, and must not treat subagent frames as turn activity.

### 3.2 CLI → host (stdout)

| `type` / `subtype` | When | Notes |
|---|---|---|
| `control_response` | reply to your `control_request` | `response.subtype` `success` (with `response.response`) or `error` (with `error`, sometimes `error_code`). |
| `control_request` / `can_use_tool` | tool needs approval (with `--permission-prompt-tool=stdio`) | §5 |
| `control_request` / `hook_callback` | a hook you registered in `initialize` fired | §6 |
| `control_request` / `mcp_message`, `elicitation`, `request_user_dialog`, … | SDK MCP servers / dialogs | **[inferred; not exercised]** |
| `system` / `init` | **at the start of every turn**, after the first `user` message is read | Not sent at process start. It arrives after `initialize` and only once the first user message comes in. |
| `system` / `session_state_changed` | `running` / `requires_action` / `idle` | Only with `CLAUDE_CODE_SDK_READS_SESSION_STATE=1`. Has `sdk_host_only:true`. |
| `system` / `status` | `{"status":"requesting"}` before each API call; `{"status":null,"permissionMode":"…"}` after a mode change | |
| `system` / `thinking_tokens` | thinking-token estimate while the model is thinking | |
| `stream_event` | raw Anthropic SSE events (`message_start`, `content_block_start/delta/stop`, `message_delta`, `message_stop`) | Only with `--include-partial-messages`. |
| `assistant` | **one frame per completed content block** (same `message.id` repeated) | `message.stop_reason` is always `null` here; the real one is in `stream_event` `message_delta` and in `result.stop_reason`. |
| `user` | tool results, interrupt markers, local-command replays | `isReplay:true` on replays. |
| `rate_limit_event` | after API calls | Subscription windows. |
| `result` | **end of every turn** (one per user message) | §8 |

#### Examples

`system/init` (fixture `multiturn`, arrays shortened):

```json
{"type":"system","subtype":"init","cwd":"/home/user/even-cc/.test/cli-probe/work","session_id":"0f2bc6db-743d-4852-a591-66adc0da37b4","tools":["Task","AskUserQuestion","…28 more"],"mcp_servers":[],"model":"claude-haiku-5-5","permissionMode":"default","slash_commands":["deep-research","design","…52 more"],"terminal_slash_commands":["doctor","color","…2 more"],"apiKeySource":"none","claude_code_version":"2.1.312","output_style":"default","agents":["claude","Explore","…3 more"],"skills":["deep-research","design","…17 more"],"plugins":[{"name":"cc-plugin-sec-default","path":"builtin","source":"cc-plugin-sec-default@builtin"},"…"],"capabilities":["interrupt_receipt_v1","interrupt_cancel_queued_v1","interrupt_send_now_v1","interrupt_requeue_taken_v1","msg_lifecycle_v1","request_marker_lists_v1","ask_user_question_unanswered_v1","lifecycle_wakeup_source_v1","sdk_mcp_tools_list_changed","sdk_mcp_manifests","mcp_read_resource_v1","mcp_tool_ui_meta_v1","ui_surface_v1","mcp_call_result_not_for_model_v1"],"analytics_disabled":false,"product_feedback_disabled":false,"uuid":"89ca68e3-f5c0-4447-a153-7c408485d535","memory_paths":{"auto":"/home/user/.claude/projects/-home-user-even-cc/memory/"},"original_cwd":"/home/user/even-cc/.test/cli-probe/work","additional_directories":[],"messaging_socket_path":"/tmp/cc-socks/2794119.sock","fast_mode_state":"off","fast_mode_disabled_reason":"sdk_opt_in_required","per_turn_effort_active":true,"view_mode":"default"}
```

Use `capabilities` for feature detection, not version sniffing.

`session_state_changed`, `status`, `thinking_tokens`:

```json
{"type":"system","subtype":"session_state_changed","state":"running","sdk_host_only":true,"uuid":"6e6876cb-5b5f-4bc3-997e-13ef290abfb4","session_id":"0f2bc6db-743d-4852-a591-66adc0da37b4"}
{"type":"system","subtype":"status","status":"requesting","session_id":"0f2bc6db-743d-4852-a591-66adc0da37b4","uuid":"db1e775b-f52e-431f-9e1c-864adc4489c3"}
{"type":"system","subtype":"status","status":null,"permissionMode":"acceptEdits","uuid":"7ac50fc0-c9c3-4ccf-8846-ee72a858668a","session_id":"a7ea49cf-e1c5-440c-a1f0-31342b2a0d1b"}
{"type":"system","subtype":"thinking_tokens","estimated_tokens":50,"estimated_tokens_delta":50,"session_id":"36380369-be11-4e78-96b6-cc337414e1bf","uuid":"4ce597d5-4334-4c2d-9513-e5d03a1b2d29"}
```

`stream_event` (text, tool_use, thinking). Every frame carries `session_id`, `parent_tool_use_id` (non-null inside
subagents), `uuid` and `api_message_id`:

```json
{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}},"session_id":"9fc02e3f-58d5-48a4-8a9a-1e8d97236723","parent_tool_use_id":null,"uuid":"a9f1dad1-8c74-449a-9fef-a6cbfef3df55","api_message_id":"msg_011CgY16zAmyNb4F57qBzHMX"}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}},"session_id":"9fc02e3f-58d5-48a4-8a9a-1e8d97236723","parent_tool_use_id":null,"uuid":"416ea51b-f6c5-4041-92ea-b89056a0b613","api_message_id":"msg_011CgY16zAmyNb4F57qBzHMX"}
{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_018Y73Tkobof2YhAUnQFRpGj","name":"Write","input":{},"caller":{"type":"direct"}}},"session_id":"36380369-be11-4e78-96b6-cc337414e1bf","parent_tool_use_id":null,"uuid":"a792cbf1-2c44-423e-a60c-780a4def759b","api_message_id":"msg_011CgY19Lu1GXY1fcDWLU5Q8"}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\": \"/home/a"}},"session_id":"36380369-be11-4e78-96b6-cc337414e1bf","parent_tool_use_id":null,"uuid":"8cdb449e-afcf-43ce-b493-e172ae6f8778","api_message_id":"msg_011CgY19Lu1GXY1fcDWLU5Q8"}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"","estimated_tokens":50}},"session_id":"36380369-be11-4e78-96b6-cc337414e1bf","parent_tool_use_id":null,"uuid":"8138c5a7-b673-419c-87d0-5c6675c0bf7d","thinking_display":"updates","api_message_id":"msg_011CgY19PpsDkg8M3w6Y7PGf"}
{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null,"stop_details":null,"container":null},"usage":{"input_tokens":2,"cache_creation_input_tokens":4292,"cache_read_input_tokens":16017,"output_tokens":96,"output_tokens_details":{"thinking_tokens":0},"iterations":["…"]},"context_management":{"applied_edits":[]}},"session_id":"36380369-be11-4e78-96b6-cc337414e1bf","parent_tool_use_id":null,"uuid":"3e944ffe-e4d6-4f47-9538-812fbed14030","api_message_id":"msg_011CgY19Lu1GXY1fcDWLU5Q8"}
```

Thinking text came through empty (`"thinking":""`, `thinking_display:"updates"`); only estimates and the signature
are sent by default. `--thinking-display summarized` may change this **[inferred]**.

`assistant` (text, then tool_use). Note that the `assistant` frame arrives *before* that block's
`content_block_stop` event:

```json
{"type":"assistant","message":{"model":"claude-haiku-5-5","id":"msg_011CgY16zAmyNb4F57qBzHMX","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"container":null,"stop_reason":null,"stop_sequence":null,"stop_details":null,"usage":{"input_tokens":2,"cache_creation_input_tokens":11160,"cache_read_input_tokens":12079,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":11160},"output_tokens":4,"service_tier":"standard","inference_geo":"global","speed":"standard"},"input_transformations":[],"diagnostics":null,"context_management":null},"parent_tool_use_id":null,"session_id":"9fc02e3f-58d5-48a4-8a9a-1e8d97236723","uuid":"75b5027f-09de-4e47-ae0e-04c61b3246b9","timestamp":"2026-10-30T07:09:39.952Z","request_id":"req_011CgY16ynhkVRTsnM2nG18k"}
{"type":"assistant","message":{"model":"claude-haiku-5-5","id":"msg_011CgY19Lu1GXY1fcDWLU5Q8","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_018Y73Tkobof2YhAUnQFRpGj","name":"Write","input":{"file_path":"/home/user/even-cc/.test/cli-probe/work/x.txt","content":"hi"},"caller":{"type":"direct"}}],"…":"…"},"parent_tool_use_id":null,"session_id":"36380369-be11-4e78-96b6-cc337414e1bf","uuid":"…"}
```

`user` frames emitted by the CLI:

```json
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_015WWAcgZq2RtvdAz9vRujM3","type":"tool_result","content":"File created successfully at: /home/user/even-cc/.test/cli-probe/work/x.txt (file state is current in your context — no need to Read it back)"}]},"parent_tool_use_id":null,"session_id":"36380369-be11-4e78-96b6-cc337414e1bf","uuid":"64fbfa20-e48b-4e81-a6db-a49a49ee040c","…":"…"}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]},"parent_tool_use_id":null,"session_id":"8b614b93-8b8a-48b5-938b-683245ea4da0","uuid":"e8865c13-50fa-40de-9d72-c41d84bca99f","timestamp":"2026-10-30T07:10:37.909Z"}
{"type":"user","message":{"role":"user","content":"<local-command-stdout>Set model to `claude-opus-5-5`</local-command-stdout>"},"session_id":"a7ea49cf-e1c5-440c-a1f0-31342b2a0d1b","parent_tool_use_id":null,"uuid":"b30f4260-595f-4d8c-a798-36cd6640ef93","timestamp":"2026-10-30T07:12:00.272Z","isReplay":true}
```

`tool_result` frames also carry `tool_use_result` (structured tool output) and `tool_result_meta` (see §5).

`rate_limit_event`:

```json
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1793356800,"rateLimitType":"five_hour","overageStatus":"allowed","overageResetsAt":1793491200,"isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.2,"resetsAt":1793356800,"status":"allowed"},"seven_day":{"utilization":0.24,"resetsAt":1793754000,"status":"allowed"}}},"uuid":"023128b3-549f-47d0-ae8b-1b3fe144c967","session_id":"9fc02e3f-58d5-48a4-8a9a-1e8d97236723"}
```

---

## 4. Control protocol

### 4.1 Envelopes and correlation

```json
{"type":"control_request","request_id":"<id>","request":{"subtype":"…", "…":"…"}}
{"type":"control_response","response":{"subtype":"success","request_id":"<id>","response":{ … }}}
{"type":"control_response","response":{"subtype":"error","request_id":"<id>","error":"<text>","error_code":"<optional>"}}
{"type":"control_cancel_request","request_id":"<id>"}
```

- `request_id` is chosen by the **sender** and must be unique among the sender's in-flight requests. The SDK uses
  `Math.random().toString(36).substring(2,15)`. The CLI uses UUIDs (`"72de1d4c-0ae2-474f-8951-07ed5c58670b"`).
  The probes used `req_1`, `req_2`, …; any string works.
- Both sides originate requests, so keep two maps: *pending outgoing* (resolve on `control_response`) and
  *in-flight incoming* (each needs exactly one reply from you).
- Note the nesting: `request_id` sits at top level in `control_request`, but **inside `response`** in
  `control_response`.
- Replies may interleave with other frames. A `system/status` frame arrived between a request and its response.
- An `initialize` success response also carries `pending_permission_requests` and `pending_user_dialog_requests`
  **as siblings of `response`** (both `[]` here). On reconnect they list prompts still awaiting an answer.
- `control_cancel_request` **[inferred]**: either side may withdraw a request it originated. The receiver aborts if
  it can and may still reply; there is no reply to the cancel itself. The CLI sends one, for example, to withdraw a
  pending `can_use_tool` after an interrupt (the CLI binary's strings mention this; not reproduced). When you receive
  one, drop your UI prompt for that `request_id` and do not reply.

### 4.2 Table

| subtype | Direction | Request payload | Success `response` | Status |
|---|---|---|---|---|
| `initialize` | host→CLI | `hooks`, `systemPrompt[]`, `appendSystemPrompt`, `agents`, `jsonSchema`, `sdkMcpServers`, `title`, `skills`, `supportedDialogKinds`, `perTaskStopAffordance`, … (all optional) | see §4.3 | **verified** |
| `interrupt` | host→CLI | `{}` (opt. `cancel_queued:true`) | `{"still_queued":[]}` (+ `cancelled` if `cancel_queued`) | **verified** |
| `set_permission_mode` | host→CLI | `{"mode":"acceptEdits"}` | `{"mode":"acceptEdits"}` | **verified** |
| `set_model` | host→CLI | `{"model":"sonnet"}`; `null`/`"default"` resets | `{"model":"claude-sonnet-5-5"}` (resolved id) | **verified** |
| *(unknown)* | host→CLI | — | error `Unsupported control request subtype: <x>` | **verified** |
| `can_use_tool` | CLI→host | `tool_name`, `input`, `tool_use_id`, `display_name`, `description`, `permission_suggestions`, `requires_user_interaction`, `decision_reason*`, `blocked_path`, `agent_id`, … | `PermissionResult` (§5) | **verified** |
| `hook_callback` | CLI→host | `callback_id`, `input` (HookInput), `tool_use_id` | `SyncHookJSONOutput` (`{}` = no-op) | **verified** |
| `mcp_message` | CLI→host | `server_name`, `message` (JSON-RPC) | `{"mcp_response": <JSON-RPC reply>}` | [inferred] |
| `elicitation` | CLI→host | MCP elicitation | `{"action":"accept"\|"decline"\|"cancel",…}`; SDK default `{"action":"decline"}` | [inferred] |
| `request_user_dialog` | CLI→host | `dialog_kind`, `payload` | per kind. **Do not answer kinds you didn't declare** in `supportedDialogKinds` | [inferred] |
| `set_max_thinking_tokens`, `mcp_status`, `mcp_set_servers`, `mcp_reconnect`, `mcp_toggle`, `rewind_files`, `get_context_usage`, `get_settings`, `apply_flag_settings`, `stop_task`, `reload_plugins`, `rename_session`, `cancel_async_message`, `seed_read_state`, … | host→CLI | see `sdk.d.ts` `SDKControl*Request` | various | [inferred] |

If your host gets a CLI→host subtype it doesn't handle, reply with `subtype:"error"` and
`error:"Unsupported control request subtype: …"`, as the SDK does. **Exception: `request_user_dialog`.** The SDK stays
silent for undeclared kinds, because an error reply is discarded and leaves the dialog pending.

### 4.3 Initialize handshake

`initialize` is **optional**. Fixture `minimal` never sent one and the turn worked. You need it to register hooks or to
send the system prompt, agents, and so on, and to learn about models, account and commands before the first turn. The
SDK always sends it first and writes nothing else until it has been written. It does not wait for the response before
sending the first user message.

Sent (fixture `multiturn`):

```json
{"type":"control_request","request_id":"req_1","request":{"subtype":"initialize","hooks":{"Notification":[{"hookCallbackIds":["hook_0"]}],"PreToolUse":[{"matcher":"Write","hookCallbackIds":["hook_1"]}],"Stop":[{"hookCallbackIds":["hook_2"]}]}}}
```

Received after about 0.6 s (lists shortened; email redacted):

```json
{"type":"control_response","response":{"subtype":"success","request_id":"req_1","response":{"commands":[{"name":"deep-research","description":"Deep research harness — …","argumentHint":"","builtin":true},"…52 more"],"agents":[{"name":"claude","description":"Catch-all …"},"…4 more"],"output_style":"default","available_output_styles":["default","Proactive","Concise","Explanatory","Learning"],"user_output_styles_dir":"/home/user/.claude/output-styles","models":[{"value":"default","resolvedModel":"claude-opus-5-5","displayName":"Default (recommended)","description":"Opus 5.5 · Best for everyday, complex tasks","supportsEffort":true,"supportedEffortLevels":["low","medium","high","xhigh","max"],"supportsAdaptiveThinking":true,"supportsFastMode":true,"supportsAutoMode":true,"supportsThinkingOff":false},"…"],"account":{"email":"user@example.com","organization":"Example Org","subscriptionType":"Claude Team","apiProvider":"firstParty"},"pid":2794119,"claude_code_version":"2.1.312","current_permission_mode":"default","hooks_applied":true,"feedback_mode":{"kind":"post"},"analytics_disabled":false,"remote_control_auto_enable":true,"remote_control_auto_connect_default":false,"remote_control_available":true,"remote_control_auto_on_by_default":false,"ide_rc_auto_enable_gate":true,"fast_mode_state":"off","fast_mode_disabled_reason":"sdk_opt_in_required","session_state":"idle","capabilities":["ui_surface_v1","mcp_call_result_not_for_model_v1"]},"pending_permission_requests":[],"pending_user_dialog_requests":[]}}
```

Useful fields: `models[]` (for a model picker; `value` is what `set_model` takes), `pid`, `claude_code_version`,
`current_permission_mode`, `hooks_applied`, `session_state`. **`session_id` is not included.** It first appears in
`system/init`, after you send the first user message.

---

## 5. Permissions (`can_use_tool`)

Requires `--permission-prompt-tool=stdio` and a tool that isn't already allowed by settings, `--allowedTools`, or the
permission mode. Sequence from fixture `permission`:

```
assistant(tool_use Write) → [hook_callback PreToolUse, if registered]
→ system/session_state_changed requires_action
→ control_request can_use_tool          ← you answer
→ system/session_state_changed running
→ user(tool_result) → …
```

Request:

```json
{"type":"control_request","request_id":"72de1d4c-0ae2-474f-8951-07ed5c58670b","request":{"subtype":"can_use_tool","tool_name":"Write","display_name":"Write","input":{"file_path":"/home/user/even-cc/.test/cli-probe/work/x.txt","content":"hi"},"description":"x.txt","permission_suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}],"tool_use_id":"toolu_018Y73Tkobof2YhAUnQFRpGj"}}
```

**Deny** (the model sees `message` as an error tool_result; the turn continues):

```json
{"type":"control_response","response":{"subtype":"success","request_id":"72de1d4c-0ae2-474f-8951-07ed5c58670b","response":{"behavior":"deny","message":"User denied writing x.txt","toolUseID":"toolu_018Y73Tkobof2YhAUnQFRpGj"}}}
```

This produced:

```json
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"User denied writing x.txt","is_error":true,"tool_use_id":"toolu_018Y73Tkobof2YhAUnQFRpGj"}]},"parent_tool_use_id":null,"session_id":"36380369-be11-4e78-96b6-cc337414e1bf","uuid":"129bc2f0-7cdc-4c06-8577-4b34d4553b6f","timestamp":"2026-10-30T07:10:12.133Z","tool_use_result":"Error: User denied writing x.txt","tool_result_meta":[{"id":"toolu_018Y73Tkobof2YhAUnQFRpGj","non_execution_kind":"permission-rule","permission_decision":{"decision":"reject","source":"user_reject"}}]}
```

and `result.permission_denials`:
`[{"tool_name":"Write","tool_use_id":"toolu_018Y73Tkobof2YhAUnQFRpGj","tool_input":{…}}]`.
`deny` also accepts `"interrupt":true`, which aborts the whole turn **[inferred from sdk.d.ts]**.

**Allow + remember** (`updatedInput` is required in practice. Echo `input` back, or send an edited version):

```json
{"type":"control_response","response":{"subtype":"success","request_id":"96c7bb6a-9a7f-4c84-8ef0-2f4bfbdf490c","response":{"behavior":"allow","updatedInput":{"file_path":"/home/user/even-cc/.test/cli-probe/work/x.txt","content":"hi"},"updatedPermissions":[{"type":"addRules","rules":[{"toolName":"Write"}],"behavior":"allow","destination":"session"}],"toolUseID":"toolu_015WWAcgZq2RtvdAz9vRujM3"}}}
```

The next `Write` in the same process (`z.txt`) ran **without** a `can_use_tool` request. Fixture `askuser` and turn 3
of `permission` show a **plain allow**: the same response without `updatedPermissions`.

`PermissionUpdate` variants: `addRules` / `replaceRules` / `removeRules` (`rules:[{toolName, ruleContent?}]`,
`behavior: allow|deny|ask`), `setMode` (`mode`), `addDirectories` / `removeDirectories` (`directories`). Each takes a
`destination`: `session` | `localSettings` | `projectSettings` | `userSettings` | `cliArg`. The CLI's
`permission_suggestions` are ready-made `updatedPermissions` entries to offer as "always allow" buttons.
`toolUseID` in the response is optional; the SDK sends it.

Unanswered `can_use_tool` requests block the turn indefinitely (`state: requires_action`) **[inferred]**.

### 5.1 AskUserQuestion

This tool exists only with `--permission-prompt-tool=stdio`. It arrives as a normal `can_use_tool` with
`requires_user_interaction:true`. You **answer by allowing with `updatedInput.answers`**, a map from question text to
the chosen label (comma-separated labels for multiSelect, or free text for "Other"). Fixture `askuser`:

```json
{"type":"control_request","request_id":"5e11180c-bdac-40ae-9d3b-0f8de57f6e17","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","display_name":"AskUserQuestion","input":{"questions":[{"question":"Pick a color?","header":"Color","options":[{"label":"Red","description":"Pick red"},{"label":"Green","description":"Pick green"},{"label":"Blue","description":"Pick blue"}],"multiSelect":false}]},"tool_use_id":"toolu_01AUMneJBSCrTiZTdqd28Htc","requires_user_interaction":true}}
```

```json
{"type":"control_response","response":{"subtype":"success","request_id":"5e11180c-bdac-40ae-9d3b-0f8de57f6e17","response":{"behavior":"allow","updatedInput":{"questions":[{"question":"Pick a color?","header":"Color","options":[{"label":"Red","description":"Pick red"},{"label":"Green","description":"Pick green"},{"label":"Blue","description":"Pick blue"}],"multiSelect":false}],"answers":{"Pick a color?":"Green"}},"toolUseID":"toolu_01AUMneJBSCrTiZTdqd28Htc"}}}
```

Resulting tool_result (the model then replied "Green"):

```json
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"Your questions have been answered: \"Pick a color?\"=\"Green\". You can now continue with these answers in mind.","tool_use_id":"toolu_01AUMneJBSCrTiZTdqd28Htc"}]},"parent_tool_use_id":null,"session_id":"86654f73-bdfd-458f-9a0f-7849c45c44f9","uuid":"82419eaf-7a03-4a7f-8080-86a14f477a94","timestamp":"2026-10-30T07:10:26.030Z","tool_use_result":{"questions":["…"],"answers":{"Pick a color?":"Green"}},"tool_result_meta":[{"id":"toolu_01AUMneJBSCrTiZTdqd28Htc","permission_decision":{"decision":"accept","source":"user_temporary"}}]}
```

Optional in `updatedInput` **[inferred from sdk-tools.d.ts]**: `annotations: {<question>: {notes?, preview?,
unanswered?: "skipped"|"replied_in_chat"|"dismissed"}}`. `unanswered` is only allowed when `system/init.capabilities`
contains `ask_user_question_unanswered_v1`, which it does in 2.1.312. Replying `deny` with a message is the way to
cancel the question **[inferred]**.

`ExitPlanMode` (plan approval) uses the same `can_use_tool` channel **[inferred]**.

---

## 6. Hooks

Register hooks in `initialize.hooks`: `{ "<HookEvent>": [ {"matcher"?: "<tool regex>", "hookCallbackIds": ["<your id>", …], "timeout"?: <sec>} ] }`.
The ids are opaque and chosen by you (the SDK uses `hook_0`, `hook_1`, …). Events (2.1.312 `HookEvent`): `PreToolUse`,
`PostToolUse`, `PostToolUseFailure`, `PostToolBatch`, `Notification`, `UserPromptSubmit`, `UserPromptExpansion`,
`SessionStart`, `SessionEnd`, `Stop`, `StopFailure`, `SubagentStart`, `SubagentStop`, `PreCompact`, `PostCompact`,
`PreModelSwitch`, `PostModelSwitch`, `PermissionRequest`, `PermissionDenied`, `Setup`, `TeammateIdle`, `TaskCreated`,
`TaskCompleted`, `Elicitation`, `ElicitationResult`, `ConfigChange`, `WorktreeCreate`, `WorktreeRemove`,
`InstructionsLoaded`, `CwdChanged`, `FileChanged`, `DirectoryAdded`, `MessageDisplay`.

When a hook fires, the CLI sends a `hook_callback` and **waits for your reply** before continuing. Reply `{}` for
"no opinion".

```json
{"type":"control_request","request_id":"9114e19d-fd2e-48f8-a543-f667545c1875","request":{"subtype":"hook_callback","callback_id":"hook_1","input":{"session_id":"36380369-be11-4e78-96b6-cc337414e1bf","transcript_path":"/home/user/.claude/projects/-home-user-even-cc--test-cli-probe-work/36380369-be11-4e78-96b6-cc337414e1bf.jsonl","cwd":"/home/user/even-cc/.test/cli-probe/work","prompt_id":"dc5f82ba-1dcf-4dfd-b04c-61bfb205d9f5","permission_mode":"default","effort":{"level":"medium"},"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"/home/user/even-cc/.test/cli-probe/work/x.txt","content":"hi"},"tool_use_id":"toolu_018Y73Tkobof2YhAUnQFRpGj"},"tool_use_id":"toolu_018Y73Tkobof2YhAUnQFRpGj"}}
{"type":"control_response","response":{"subtype":"success","request_id":"9114e19d-fd2e-48f8-a543-f667545c1875","response":{}}}
```

`Stop` fires once per turn, just before `result`:

```json
{"type":"control_request","request_id":"29a39d19-f79a-4212-8e3e-93094fda422b","request":{"subtype":"hook_callback","callback_id":"hook_2","input":{"session_id":"0f2bc6db-743d-4852-a591-66adc0da37b4","transcript_path":"/home/user/.claude/projects/-home-user-even-cc--test-cli-probe-work/0f2bc6db-743d-4852-a591-66adc0da37b4.jsonl","cwd":"/home/user/even-cc/.test/cli-probe/work","prompt_id":"3041ef07-d1c3-4b00-8e75-9ad5960621bf","permission_mode":"default","effort":{"level":"medium"},"hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":"OK","background_tasks":[],"session_crons":[]},"tool_use_id":"fd9ec59d-a708-4493-a149-2211cc4a0d9a"}}
```

The hook output shape (`SyncHookJSONOutput`) is: `continue`, `suppressOutput`, `stopReason`,
`decision: approve|block`, `systemMessage`, `reason`, `hookSpecificOutput: {hookEventName, …}`. For example, a
PreToolUse can set `hookSpecificOutput.permissionDecision` **[inferred]**. Async form: `{"async":true,"asyncTimeout":ms}`
**[inferred]**.

**Notification:** registered (`hook_0`) in three runs and **never fired**, not even while a `can_use_tool` prompt was
pending. Input shape **[inferred]**: `{hook_event_name:"Notification", message, title?, notification_type}`. In
stream-json mode, do not rely on Notification to learn about permission prompts. `can_use_tool` and
`session_state_changed: requires_action` are the signals for that.

---

## 7. Turn sequencing (observed)

**Text-only turn** (fixture `multiturn`, turn 1):

```
→ control_request initialize            (optional, once)
← control_response success
→ user
← system/session_state_changed running
← system/init                           (every turn)
← system/status requesting
← stream_event message_start … content_block_delta* …
← assistant                             (per completed block)
← stream_event content_block_stop, message_delta, message_stop
← rate_limit_event                      (position varies)
← control_request hook_callback (Stop)  → you reply
← result
← system/session_state_changed idle     ← the turn is truly over; safe to send the next user message
```

`result` comes *before* `idle`. The SDK treats a turn as over at `result` when session-state is off, otherwise at
`idle`.

**Tool turn with approval:** see §5. One user turn carries several API round-trips (`status requesting` before each),
and `result.num_turns` counts them (2 for one tool call).

**Interrupt** (fixture `interrupt`). Sent about 1.1 s into a long count:

```
→ {"type":"control_request","request_id":"req_2","request":{"subtype":"interrupt"}}
← {"type":"control_response","response":{"subtype":"success","request_id":"req_2","response":{"still_queued":[]}}}
← assistant           (partial text so far: "1\n2\n3\n4")
← user                {"type":"text","text":"[Request interrupted by user]"}
← stream_event content_block_stop / message_delta / message_stop
← result              subtype "error_during_execution", is_error true, terminal_reason "aborted_streaming"
← system/session_state_changed idle
```

```json
{"duration_api_ms":628,"stop_reason":null,"session_id":"8b614b93-8b8a-48b5-938b-683245ea4da0","total_cost_usd":0.00012790000000000002,"usage":"…","modelUsage":"…","permission_denials":[],"terminal_reason":"aborted_streaming","fast_mode_state":"off","fast_mode_disabled_reason":"sdk_opt_in_required","subagent_stats":"…","safety_stops":0,"is_error":true,"num_turns":2,"subtype":"error_during_execution","errors":["[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=null"],"type":"result","duration_ms":1115,"uuid":"ccb3bac0-33e4-40bf-817a-418f93068ac2","queued_turn_count":0,"result_index":0,"model_usage_since_last_result":"…"}
```

The process stays alive, and the next `user` message ran normally (`"OK"`). An interrupted turn yields an `is_error`
result, so treat `terminal_reason:"aborted_streaming"` as "user stopped" rather than a failure. Interrupting while
idle: **[inferred]** success no-op.

**Interrupt and background tasks** (probe `.test/probe/interrupt_bg.py`, 2.1.312, 2026-10-30). Without
`perTaskStopAffordance` in `initialize`, `interrupt` **stops every running background agent and its shells**
(`task_notification` `status:"stopped"` within 10 ms), both when sent after the launch turn's `result` (CLI still
`running` for the agent, no turn to stop; the CLI then goes `idle`) and when sent during a later foreground turn
(which ends `aborted_tools`). With `"perTaskStopAffordance":true` (described in the binary as "an interrupt on an
open-input session spares running background agents/workflows (Stop only aborts the turn)"), the agent survived
both and finished `completed`; a foreground turn still ends `error_during_execution` / `aborted_tools`. The flag
also declares that the host can stop one task with `{"subtype":"stop_task","task_id":…}`. even-cc sets it and never
sends `interrupt` between turns.

---

## 8. `result` semantics

Full success result (fixture `minimal`; note the key order):

```json
{"duration_api_ms":559,"stop_reason":"end_turn","session_id":"9fc02e3f-58d5-48a4-8a9a-1e8d97236723","total_cost_usd":0.00235499,"usage":{"input_tokens":2,"cache_creation_input_tokens":11160,"cache_read_input_tokens":12079,"output_tokens":4,"output_tokens_details":{"thinking_tokens":0},"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":11160,"ephemeral_5m_input_tokens":0},"inference_geo":"global","iterations":["…"],"speed":"standard","fallback_credit":null},"modelUsage":{"claude-haiku-5-5":{"inputTokens":2,"outputTokens":4,"cacheReadInputTokens":12079,"cacheCreationInputTokens":11160,"webSearchRequests":0,"costUSD":0.00235499,"contextWindow":1000000,"maxOutputTokens":128000,"thinkingTokens":0,"cacheCreationEphemeral5mInputTokens":0,"cacheCreationEphemeral1hInputTokens":11160,"canonicalModel":"claude-haiku-5-5","provider":"firstParty","costBasis":"list"}},"permission_denials":[],"terminal_reason":"completed","fast_mode_state":"off","fast_mode_disabled_reason":"sdk_opt_in_required","subagent_stats":{"spawned":0,"…":"…"},"safety_stops":0,"is_error":false,"num_turns":1,"subtype":"success","api_error_status":null,"result":"OK","ttft_ms":769,"type":"result","duration_ms":797,"uuid":"5e13cdeb-9f2b-450d-b625-5c4a6ffb4656","ttft_stream_ms":769,"time_to_request_ms":240,"first_content_frame_ms":769,"queued_turn_count":0,"result_index":0,"model_usage_since_last_result":{"claude-haiku-5-5":{"…":"…"}}}
```

Accumulation across turns on one process (fixtures `multiturn` and `permission`):

| Field | Scope | Evidence |
|---|---|---|
| `num_turns` | **per user turn** (API round-trips in this turn) | 1, 1 (multiturn); 2, 2, 2 (permission) |
| `total_cost_usd` | **cumulative for the process/session** | 0.00159 → 0.00312; 0.00205 → 0.00331 → 0.00398 |
| `duration_api_ms` | **cumulative** | 1542 → 2300 |
| `duration_ms` | per turn | 1130 → 780 |
| `usage`, `stop_reason`, `result` | per turn (last API call) | |
| `modelUsage` | cumulative **[inferred: mirrors total_cost_usd]** | |
| `model_usage_since_last_result` | per turn | |
| `result_index` | 0, 1, 2, … per process | |
| `permission_denials` | per turn | |

Per-turn cost = `total_cost_usd` minus the previous result's value (or sum `model_usage_since_last_result[*].costUSD`).
After `set_model sonnet`, one "Reply OK" cost $0.025, versus about $0.0016 on haiku.

`subtype` values: `success`, `error_during_execution`, `error_max_turns`, `error_max_budget_usd`,
`error_max_structured_output_retries`. On errors, read `errors: string[]`. A success can still have `is_error:true`
(API error text in `result`) **[inferred from SDK `readMessages`]**.

---

## 9. `set_permission_mode` / `set_model` (fixture `modes`)

```json
→ {"type":"control_request","request_id":"req_2","request":{"subtype":"set_permission_mode","mode":"acceptEdits"}}
← {"type":"control_response","response":{"subtype":"success","request_id":"req_2","response":{"mode":"acceptEdits"}}}
← {"type":"system","subtype":"status","status":null,"permissionMode":"acceptEdits","uuid":"7ac50fc0-c9c3-4ccf-8846-ee72a858668a","session_id":"a7ea49cf-e1c5-440c-a1f0-31342b2a0d1b"}

→ {"type":"control_request","request_id":"req_3","request":{"subtype":"set_permission_mode","mode":"bogus"}}
← {"type":"control_response","response":{"subtype":"error","request_id":"req_3","error":"Cannot set permission mode: must be one of acceptEdits, auto, bypassPermissions, default, dontAsk, plan","error_code":"invalid_mode"}}

→ {"type":"control_request","request_id":"req_5","request":{"subtype":"set_model","model":"sonnet"}}
← {"type":"control_response","response":{"subtype":"success","request_id":"req_5","response":{"model":"claude-sonnet-5-5"}}}
   (next assistant message: "model":"claude-sonnet-5-5")

→ {"type":"control_request","request_id":"req_6","request":{"subtype":"set_model","model":"default"}}
← {"type":"user","message":{"role":"user","content":"<local-command-stdout>Set model to `claude-opus-5-5`</local-command-stdout>"},…,"isReplay":true}
← {"type":"control_response","response":{"subtype":"success","request_id":"req_6","response":{"model":"claude-opus-5-5"}}}
```

Both work while idle and **between** turns, and take effect on the next API call. Changing them mid-turn was not
tested **[inferred: allowed]**. `"default"` resolves to the account default (Opus 5.5 here; user settings were not
loaded). Expect the `isReplay` user frame on some `set_model` calls, and filter it out of the chat view.

---

## 10. Shutdown, exit and errors

### 10.1 Graceful: close stdin

- **Idle:** exits with code 0 about 0.7 s after EOF (`minimal`, `multiturn`, `permission`, …).
- **Mid-turn** (fixture `eofmidturn`): the CLI **finishes the current turn**, emitting the rest of the stream,
  `result` (success) and `idle`, then exits 0. EOF means "no more input", not "abort". To abort, send `interrupt`
  first, wait for `result`, then close stdin.

### 10.2 Signals

- **SIGTERM mid-turn** (fixture `killmidturn`): exit **code 143** (`code:143, signal:null`, meaning the CLI handles
  SIGTERM and exits 128+15). The streamed output after the signal and a `result success` were still flushed. This
  probably happened because haiku had already finished generating (the turn ended 300 ms after the signal), so
  **whether SIGTERM waits for a long turn is not established [inferred]**. Treat 143 as a normal "terminated" exit.
- SIGKILL: no output, killed by signal **[inferred]**.

### 10.3 What the SDK does (`ProcessTransport.close()`)

1. `stdin.end()`.
2. After **2000 ms** (`p6e`), if the process has not exited: `SIGTERM`.
3. After a further **5000 ms**: `SIGKILL`.

On process `exit`, the SDK waits for stderr to drain (up to 200 ms, `f6e`), then reports:
- code ≠ 0: `Claude Code process exited with code N. stderr: <tail>`
- signal: `Claude Code process terminated by signal SIG…`
- If the last `result` was an error, the SDK reports that result text instead of the exit error.

Writes after exit fail with `Cannot write to terminated process`. Guard writes with an "alive" flag, and handle
`EPIPE` on stdin.

A Go mirror of this: `stdin.Close()`; `select` on `cmd.Wait()` with a 2 s timer, then
`cmd.Process.Signal(SIGTERM)`, then a 5 s timer, then `Kill()`.

### 10.4 Error cases

| Case | Observed behaviour |
|---|---|
| Missing `--verbose` | stderr `Error: When using --print, --output-format=stream-json requires --verbose`, exit 1, no stdout. |
| Malformed JSON line on stdin | stderr `Error parsing streaming input line (type=unknown, 9 chars): SyntaxError`, **exit 1 immediately**. Pending requests are never answered. |
| Unknown control subtype | `control_response` error `Unsupported control request subtype: no_such_subtype`; the process stays alive. |
| Invalid argument (mode) | `control_response` error with `error_code:"invalid_mode"`; the process stays alive. |
| Interrupt | `result` `error_during_execution` / `aborted_streaming`; the process stays alive. |
| Tool denied | error `tool_result` + `result.permission_denials`; the turn continues. |
| API / auth failure | `result` with `is_error:true` (and possibly `api_error_status`, `startup_failure_reason`), or a non-zero exit with the reason on stderr **[inferred]**. |
| Unanswered `can_use_tool` / `hook_callback` | the turn blocks; `requires_action` **[inferred]**. |

---

## 11. Version-sensitive notes

- The field sets of `system/init`, the `initialize` response, and `result` change between releases. In 2.1.312, result
  gained `ttft_ms`, `result_index`, `model_usage_since_last_result`, `subagent_stats`, `terminal_reason`, and so on.
  Decode tolerantly and ignore unknown fields.
- Use `system/init.capabilities` (e.g. `interrupt_receipt_v1`, `interrupt_cancel_queued_v1`,
  `ask_user_question_unanswered_v1`) for feature detection.
- `pending_permission_requests` / `pending_user_dialog_requests` on the `initialize` response are guaranteed only from
  CLI ≥ 2.1.268 (per `sdk.d.ts`).
- `session_state_changed` is opt-in through `CLAUDE_CODE_SDK_READS_SESSION_STATE=1`.
- AskUserQuestion, EnterPlanMode and ExitPlanMode exist only with `--permission-prompt-tool=stdio` (verified in 2.1.312).
- The SDK package is versioned separately (0.3.314) from the CLI (2.1.312). The protocol has no explicit version
  number. Your user settings (allow rules, MCP servers, remote-control flags) change behaviour through
  `--setting-sources`.
- The `assistant` frame repeats `message.id` once per content block, with `stop_reason:null`. Do not assume one frame
  per API message.

---

## 12. Subagent tasks and context usage (verified 2026-10-30, 2.1.312)

Probe: `.test/cli-probe/probe.mjs subagent` (one foreground and one background Agent call in parallel,
`initialize` with `"agentProgressSummaries":true`, then `get_context_usage`).

- `system/task_started`: `task_id`, `run_id`, `tool_use_id` (the Agent call), `description`,
  `subagent_type`, `is_backgrounded`, `spawn_depth`, `task_type:"local_agent"`, `prompt`. Shell commands
  run *by* the subagent emit their own `task_started` with `task_type:"local_bash"`,
  `owned_by_subagent:true` and `parent_task_id` = the subagent's task id.
- `system/task_progress`: after each subagent tool call. `usage:{total_tokens,tool_uses,duration_ms}`,
  `last_tool_name`. **`description` here is the current activity** (`"Running Sleep 12 seconds…"`),
  not the original description. With `agentProgressSummaries`, a frame with `summary` (a short
  present-tense line, e.g. `"Waiting 12 seconds in probe-work"`) arrived ~31 s into the subagent.
- `system/task_updated`: `patch:{status:"completed",end_time}` right before the notification.
- `system/task_notification`: `status` (`completed|failed|stopped`), `summary` (the subagent's final
  report text), `usage`, `output_file`. For a **foreground** agent it arrives ~4 ms *before* the Agent
  `tool_result`. For a **background** agent the Agent `tool_result` comes at once
  (`"Async agent launched successfully…"`) and the notification when it ends, possibly after `result`.
- `get_context_usage` (`detail:"summary"` or `"full"`): `{percentage:3,totalTokens:27483,maxTokens:1000000,rawMaxTokens:1000000,model:"claude-haiku-5-5"}`
  after the turn. The last main-agent message's usage gives the same number:
  `input 2 + cache_read 20540 + cache_creation 6941 + output 224 − thinking 221 = 27486`;
  `result.modelUsage[model].contextWindow` was `1000000`.

---

## Fixtures

`testdata/claude/*.jsonl`. One record per line: `{"t": <ms since spawn>, "dir": "send"|"recv"|"stderr"|"exit"|"note", "data": <object or string>}`.
`send` = what was written to stdin. `recv` = a parsed stdout line. The first `note` holds the exact argv, the env key
names, and the cwd. The account email is redacted to `user@example.com`. Model: `haiku` unless switched. All runs were
new sessions in `/home/user/even-cc/.test/cli-probe/work`. The probe driver is
`/home/user/even-cc/.test/cli-probe/probe.mjs` (`node probe.mjs <scenario>`).

| File | Env | Shows |
|---|---|---|
| `noverbose.jsonl` | bare | missing `--verbose` → exit 1 |
| `minimal.jsonl` | bare (`env -i`) | no `initialize`, one turn, partial messages, no session_state, EOF exit 0 |
| `multiturn.jsonl` | SDK-like | initialize with hooks + response, two turns, Stop hook, cumulative cost |
| `permission.jsonl` | SDK-like | PreToolUse hook, can_use_tool deny → allow+`updatedPermissions` → no prompt |
| `askuser.jsonl` | SDK-like | AskUserQuestion via can_use_tool + `answers` |
| `interrupt.jsonl` | SDK-like | interrupt mid-stream, then a normal turn |
| `modes.jsonl` | SDK-like | set_permission_mode (ok/invalid), unknown subtype, set_model sonnet/default |
| `badjson.jsonl` | SDK-like | same start as modes, then a malformed stdin line → exit 1 |
| `eofmidturn.jsonl` | SDK-like | stdin closed mid-stream → turn completes, exit 0 |
| `killmidturn.jsonl` | SDK-like | SIGTERM mid-stream → exit code 143 |

"SDK-like" env = `HOME PATH USER LANG` + `CLAUDE_CODE_ENTRYPOINT=sdk-ts` + `CLAUDE_CODE_SDK_READS_SESSION_STATE=1`.
SDK-like runs used `--permission-prompt-tool=stdio --setting-sources=project`, except `eofmidturn` and `killmidturn`,
which omitted the prompt tool.
