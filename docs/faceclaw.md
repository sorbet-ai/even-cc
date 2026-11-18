# Faceclaw (custom G2 firmware + phone app): research notes

Researched 2026-10-30 from source clones (faceclaw `bc57557` 2026-10-26, g2flash `ca7e0b7` 2026-10-05, g2mirror `a6ff044`
2026-09-23), GitHub issues/releases (`gh api`) and web search. Nothing was installed or run on glasses.
"Unverified" means no primary source was found.

## Bottom line

Worth one deliberate experiment, not a default. The payoff is real: the custom firmware (CFW) raises BLE from about
3 KiB/s (stock idle) or 12 KiB/s (stock fast) to about 41 KiB/s, sends dirty-rect, compressed, glyph-cached frames, and
gives a 640x480 4-bit canvas with any font. The terminal app shows roughly 100x35 cells of Terminus 6x12.
Risk for a user installing the released build: low to moderate. Flashing uses Even's own OTA path, only the main-app
component is patched, uninstall and the official Even OTA both restore stock, and I found no brick reports. Against it:
(a) a CFW that crashes on boot cannot take an OTA, and no recovery for a dead lens is documented; (b) it voids the
warranty; (c) the CFW is frozen on a 2.3.0.24 base, so glasses on stock newer than 2.3.2.14 (likely after Even's
2.3.2/2.4.0 updates) get an "unvalidated" downgrade that needs an override; (d) on Faceclaw, Even's Terminal Mode, and
so even-cc as it stands, is unusable (one BLE slot; Faceclaw does not speak the even-terminal protocol).
Best integration: give even-cc a g2mirror-compatible websocket endpoint, so Faceclaw's Terminal app shows it unchanged.

## 1. What it is (architecture)

- **Patches the firmware; it does not replace it.** `app/g2/firmware/cfw-patches.ts` (generated from
  `g2flash/patches/cfw_patches.json`) holds 38 byte patches (offset, old, new) for stock `g2_2.3.0.24.bin`. The phone
  downloads that file from `cdn.evenreal.co`, checks the stock and patched SHA-256, then flashes it. The patches are
  branch hooks into about 6.4 kLOC of injected C (`g2flash/patches/*.c`, clang to position-independent Thumb).
- Hooks in the patch list: 640x480 direct framebuffer (`display_copy_hook`), private BLE SID 0xf0 transport, LE 2M PHY
  plus a 7.5 ms interval with idle slow mode removed, ring/temple gestures, head-up/wear, compass, ALS, mic, wake-word
  takeover, and settings field 100 reporting `Faceclaw/<rev>`. Takeovers are "fail-open" and gated by a framebuffer
  "lease": with no lease the stock UI runs.
- **Glasses**: stock firmware plus hooks, a display-list interpreter, a 256 KB texture/glyph cache, zlib transport.
  **Phone**: the Faceclaw app (NativeScript TypeScript UI, Kotlin BLE/compositor, shared Kotlin Multiplatform). It is the
  whole OS: launcher, window manager, apps, voice assistant, EvenHub emulation. **PC (optional)**: `g2mirror` (Rust)
  wraps a CLI in a pty, and `g2mirror-server` is the websocket gateway the phone dials (e.g. over a tailnet).
  `g2flash.py` is a standalone Python flasher (bleak, or a phone bridge) that writes the same image.
- **Platforms (corrects `evenhub-sdk.md` §8)**: no longer Android only. APKs ship on GitHub Releases, there is an iOS
  TestFlight beta ("first public build pending" per install.html), and a Wear OS remote.

## 2. Languages, size, quality

- faceclaw: about 111 kLOC TypeScript, 47 kLOC Kotlin, 192 test files (node:test `tests/*.test.cjs` plus Kotlin tests).
  CI runs `npm test`, `test:kotlin`, `check`, oxlint and gitleaks. Builds with NativeScript 9, SDK 35, JDK 21; Wear OS
  is a separate Gradle build.
- g2flash: about 950-line Python flasher plus C patches; `audit_stock.py` checks stock bytes and hashes are pinned.
  g2mirror: about 7 kLOC Rust (tokio, vt100, tungstenite) with 5 integration tests.
- Verdict: unusually solid for a hobby project. Frames are composited on the phone as 4-bit grey; Kotlin `TexturePlanner`
  computes dirty rects; glyphs and icons are registered once in an on-glasses atlas and drawn by reference; compression
  runs at the transport layer and across messages; display lists animate on the glasses with no per-frame traffic.
  Heavily LLM-assisted (`CLAUDE.md`, commits by "AI agent (Serac)", a commit "Clean up a bunch of slop the LLMs added").
- Frame-rate claims: "reasonably handle 30fps full-screen animation" (CHANGELOG 0.7.0); Flappy ~12 fps and Pinball
  ~9 fps (source comments); terminal repaints coalesce at ~30 fps. The method is `g2flash/demos/video-bench.ts` (streams
  GIF frames paced on ACKs, prints fps); no published result. **The "~22 fps" figure has no source anywhere (unverified).**
- BLE numbers from the author (issue #8): stock fast about 12 KiB/s, stock slow (after 60 s) about 3 KiB/s, CFW about
  41 KiB/s; the EM9305 radio could in theory do about 200 KiB/s. Release 0.7.0 claims 3-10x faster.

## 3. Flashing risk

- **Mechanism**: OTA over BLE. Kotlin `OtaFlashFlow.kt` ports `g2flash.py`: the stock EVENOTA container in 4 KB blocks,
  CRC32C per component, after the stock sid-0x80 auth handshake, left lens then right. The user confirms on the lens and
  both arms need 30% battery or more. Only `ota/s200_firmware_ota.bin` is patched; no cable, DFU or bootloader change.
  App text says each lens "takes a few minutes"; a fork measured about 17 min for a full serial flash on stock BLE (g2flash #3).
- **Brick risk**: g2flash README: "WARNING — this voids your warranty and can brick the glasses" and "if you make the
  glasses crash on boot, they won't be able to accept an OTA update"; rebasing is "especially hazardous". The OTA
  updater itself is untouched and "is tested for compatibility". The separate WebFlasher says it is "kein Tool zur
  Wiederherstellung toter Bügel" (not a tool for reviving dead temples) because the case cannot write firmware to an
  unresponsive frame (mixed-news, 2026-10). So **no recovery path for a dead lens is known**.
- **Revert**: Faceclaw "Uninstall firmware" flashes unmodified stock (`buildStockFirmware`). Or accept the official Even
  app's OTA, which "will fully remove the custom firmware". g2flash can also write an unmodified image.
- **Version gate** (`app/g2/firmware-compat.ts`): stock ≤2.3.0.24 is "flashable", ≤2.3.2.14 "newer-stock-validated",
  newer "newer-stock-unvalidated" (override only). The author, 2026-10-29 (#6): stock 2.3.2 "used up almost all of the
  free space in MRAM … not planning on rebasing past 2.3.0". So installing downgrades, and you lose newer Even features
  (2.4.0: Even AI 2.0, Knowledge Base, Dark UI) while on CFW.
- **Even app on CFW**: "intended to be backwards-compatible … In practice, this isn't tested"; "EvenHub apps are
  especially likely to be broken". The Even app will offer an OTA that wipes the CFW.
- **Warranty**: void by the tools' own statement: the app asks "Flashing custom firmware will void your warranty.
  Continue?" and g2flash makes you type `my warranty is void`. I found no Even Realities statement.
- **Reports**: no bricked devices found in faceclaw/g2flash issues or on the web; Reddit and Discord turned up nothing
  (discord.gg/XVFDFSwek3 not read). Reported issues: flash error "expected 5 components, found 6" (#1, fixed), reconnect
  loops (#41/#42, fixed), ring not connecting on 0.8.2 (#43), forced fast interval starving A2DP audio (#44, open). One
  user "reverted back to official firmware due to constant disconnects" (#41).
- Side note (mixed-news, 2026-10): the independent SybilSight/WebFlasher dropped custom-firmware support; no reason given.

## 4. Maturity

- Repo created 2026-04-27; 695 commits, 660 by James Babcock (jimrandomh), so effectively one maintainer. Commits per
  month: Jul 78, Aug 173, Sep 261, Oct 137. Last commit 2026-10-26; actively maintained.
- 97 stars, 29 forks, 17 open issues/PRs. Releases 0.6 (2026-08-30) to 0.8.5 (2026-10-25), about weekly, with 2-39 APK
  downloads each (about 270 total), so likely dozens of users. g2flash 45 stars; g2mirror 8 stars, 17 commits.
- Firmware coupling: the app requires CFW revision 37 or newer, and you update the firmware with each app update ("not
  expected to be compatible with each other"), so many releases mean a reflash. Base history: 2.2.6 → 2.2.9 (0.6.3) →
  2.3.0.24 (0.7.2). Stability is improving but still beta: many connection-loop fixes, and Android background
  throttling is "fraught" (README).

## 5. Capabilities vs stock

| | Stock / Even Hub | Faceclaw |
|---|---|---|
| Canvas | 576x288 Hub area, firmware fonts | full 640x480, 4-bit grey, TTF/BDF fonts (8-28 px UI; Terminus, Roboto Mono, Inter...) |
| Speed | 3-12 KiB/s; our 288x144 tile ≈380 ms | ≈41 KiB/s, dirty-rect + glyph cache + zlib, display-list animation |
| Input | ring/temple via SDK events | R1 ring (raw press/release/swipe speed), temple long-press, Wear OS watch, phone touch/keyboard, CLI `faceclaw-input` |
| Voice | Even AI / SDK mic | "Hey Even" takeover, on-device sherpa-onnx/Whisper or cloud STT, Anthropic/OpenAI/OpenClaw/Hermes; multi-mic streaming |
| Battery | baseline | "similar to the stock Even app" (proper sleep), plus an *estimated* +5%/day for fast BLE (not measured) |
| Even app | n/a | replaces it; single BLE slot, so not simultaneous; runs EvenHub apps via emulation |
| Root | n/a | none; sideloaded APK. Broad permissions (notification listener, MANAGE_EXTERNAL_STORAGE, QUERY_ALL_PACKAGES), opt-in stats |

## 6. Fit for even-cc

- **Zero-code trial**: Faceclaw Terminal app plus `g2mirror claude`. Default grid is Terminus-12 (6x12), about 106x40 on
  the full canvas minus chrome (estimate about 100x35). Input by voice ("type into terminals"), phone keyboard or watch.
  Caveats: `terminal-emulator.ts` says color, bold and inverse are "deliberately dropped" (text only); g2mirror is
  Unix-only (`pty-process`, Unix sockets, SIGWINCH), so WSL on the user's PC; it mirrors Claude's TUI, not even-cc's view.
- **Recommended**: add a g2mirror-protocol endpoint to even-cc (PROTOCOL.md v1: websocket JSON `init`, `list` →
  `sessions`, `connect`, `view` → `snapshot`/`output` base64 VT bytes, `input`, `bell`/`activity`/`title`). even-cc
  renders its own dense transcript, tool lines and approvals as VT text sized to `init` width/height: Windows-native, no
  Faceclaw fork. Go needs a websocket implementation (golang.org/x/net/websocket or a small hand-rolled one).
- **Richer option**: fork Faceclaw and add an even-cc app worker modelled on its T3 Code client (`app/apps/t3code/`, a
  structured-transcript client) for color, inverse highlights and gauges via display lists. Needs the
  NativeScript/Android SDK build chain.
- Wall-clock effort (uncompressible only): APK install plus flash about 10-20 min (unverified; ~17 min measured on stock
  BLE), the same again to revert plus an Even OTA; g2mirror cargo build in WSL a few minutes; the even-cc endpoint is
  seconds of Go builds, then real-hardware test loops.
- Main risks: warranty and brick tail; stuck on 2.3.0-era stock; reflash per Faceclaw update; one maintainer; switching
  to Even Terminal Mode costs a reconnect (or an OTA if the Even app insists); Android background throttling.
- Check first: the firmware version in the Even app. Above 2.3.2.14, the install is an unvalidated downgrade.

## Sources

https://github.com/jimrandomh/faceclaw (README, CHANGELOG, CLAUDE.md, issues #1 #6 #8 #41-#44, releases) ·
https://github.com/jimrandomh/g2flash (README, AGENTS.md, #1 #3) · https://github.com/jimrandomh/g2mirror (README,
PROTOCOL.md) · https://faceclaw.org/ and /install.html ·
https://mixed-news.com/en/even-realities-webflasher-official-g2-r1-firmware-no-custom-builds/ ·
https://mixed-news.com/en/even-g2-panel-576x288-glossary-640x480-firmware-patch-set/ ·
https://www.androidheadlines.com/2026/10/even-realities-biggest-even-g2-update-lets-your-glasses-remember-your-work.html ·
local clones `C:\Users\user\AppData\Local\Temp\faceclaw-o0un\{faceclaw,g2flash,g2mirror,faceclaw-agent-bridge}`.
