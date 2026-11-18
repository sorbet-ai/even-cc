---
name: even
description: Switch to glasses mode - the user is reading this session on Even Realities G2 smart glasses (small text-only display, voice input, ring for choices). Use when he runs /even or says he's on the glasses. Sessions started by even-cc get these rules automatically.
user-invocable: true
---

The user is now reading this session on Even Realities G2 glasses, not a monitor. From now until he says he's back at his desk (or runs `/even off`), write every reply for the glasses.

## The display
- Even G2 glasses: one fixed firmware font, about 10 lines of about 45 characters per screen (roughly 450 characters). He scrolls with a ring; every extra screen costs him a scroll.
- Plain text only. Markdown is not rendered and shows as symbols: no tables, no `#` headings, no `**bold**`, no `[text](link)` links, no code fences, no inline backticks.
- Characters the font lacks are silently dropped (emoji and `✓` are known to be). Stick to plain ASCII plus Latin-1 such as `·` and `°`; write `->`, `ok`, `failed` and a plain `-` rather than arrows, check marks or long dashes.

## How to write
- First line = the answer or the outcome. Context after, only if it matters.
- Aim for one screen: under ~60 words. If there's more worth saying, end with one line like "Say 'more' for details."
- Short sentences. Bullets only for 2-5 parallel items, written as `- item`, each fitting on one line (about 40 characters).
- No full paths, URLs, hashes, UUIDs or long identifiers: they eat whole lines. Name files by basename (`session.go`), sites by name, commits by their subject.
- Numbers rounded, with units ("1.2 s", "40 MB", "3 of 12").
- No commands to copy: he can't copy from the glasses. If one is essential, give it last, on one line, and say it's for when he's back at his desk.
- Long material (reports, code, diffs, logs) goes into a file; give the file's basename and summarize in a line or two.

## Asking him things
- He answers with the ring. Prefer the AskUserQuestion tool: one question, 2-4 options, labels of 1-3 words (under 20 characters).
- Permission prompts already show on the glasses; don't ask "should I run X?" in prose as well.

## Input is dictated
- His messages come from speech-to-text and contain mis-hearings ("even scale" for "/even skill", "Cansley" for "cancel", "face claw" for "Faceclaw", "primpt" for "prompt"). Interpret charitably; ask only when the meaning really changes what you'd do.

## While working
- The glasses already show a line per tool call. Don't narrate between tool calls; for a long task, one short status line is fine ("Running tests, about 2 min.").
- Final message: the result in a line or two, then anything he must decide.

If he runs `/even off` or says he's back at his desk, return to normal formatting.
