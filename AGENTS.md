# AGENTS.md

## Project Overview
- Local Go CLI for read-only Telegram harvesting plus one tightly scoped Saved Messages send primitive through MTProto user authorization.
- The tool exports selected study chat data and daily personal context for downstream automation and agent reads.
- Keep runtime credentials, sessions, state, dumps, and generated agent views out of git.
- Study runtime scope is the configured study-chat allowlist; main-profile daily harvest scope is outgoing/self messages plus configured chat-scoped additional senders for one day.

## Safety
- Harvesting is a read-only hard boundary. The sole permitted mutation is `send-saved`: it must use profile `main`, verify that the authorized account is `@Pheik13`, target `InputPeerSelf`, expose no recipient argument, and read the sent message back. Do not add any other send, click, delete, pin/unpin, join, or mark-read operation.
- Keep history crawlers and high-level media selection sequential and paced. Media downloads share exactly two global Telegram chunk slots: one worker below 1 MiB, two workers from 1 MiB, so either two small files or one large file may transfer at once. History RPC sections are exclusive with downloads; production chunk concurrency remains capped at two.
- Accept Telegram performance changes only after same-corpus A/B checks preserve message keys and stable JSONL/media content with zero Telegram errors and zero FloodWait. Never promote a faster probe setting that fails those checks.
- Treat `.env`, `.sessions/`, `.state/`, dumps, and chat exports as private local data.
- Never print app hashes, passwords, session data, or full phone numbers.
- Do not keep broad dumps of other people's messages in repo-local `.state/`; daily full-dialog scans may emit only outgoing/self records and explicitly configured sender IDs scoped to their configured chat IDs.

## Commands
- Install pinned dependencies: `make setup`
- Format: `make fmt`
- Standard validation: `make check`
- Static/security audit: `make audit`
- Build reusable CLI: `make build`; Make commands rebuild `bin/telegram-harvest` only when Go/module inputs change.
- Doctor: `make doctor PROFILE=<main|study>`
- Login: `make login PROFILE=<main|study>`
- Send text to the main account's own Saved Messages: `bin/telegram-harvest --profile main send-saved --text <message>`
- Send a file to the main account's own Saved Messages: `bin/telegram-harvest --profile main send-saved --file </absolute/path> [--caption <message>]`
- Daily harvest: `make daily PROFILE=main DATE=yesterday`
- Daily catch-up through yesterday: `make daily-catchup PROFILE=main`
- List chats: `bin/telegram-harvest --profile study chats --query вшэ`
- List forum topics: `bin/telegram-harvest --profile study topics --chat <forum-id-or-username>`
- Dump chat: `bin/telegram-harvest --profile study dump --chat <id-or-username> --out chat.jsonl` (relative outputs resolve inside the selected profile state dir)
- Start full sync: `bin/telegram-harvest --profile study sync --chat <id-or-username> --name hse-main --all --reset`
- Resume interrupted full sync: rerun the same `sync --all` command without `--reset`; state keeps `backfill.next_offset_id`.
- Incremental sync after full sync completion: `bin/telegram-harvest --profile study sync --chat <id-or-username> --name hse-main`
- Compact agent view: `bin/telegram-harvest --profile study compact --in messages.jsonl --out messages.toon`
- Markdown navigation for agents: `bin/telegram-harvest --profile study agent-view --in messages.jsonl --out-dir agent-view`; it writes under the profile state dir, updates incrementally when possible, and accepts `--rebuild` for a full rewrite.

## Version control
- The user has given standing approval to push completed Telegram Harvest commits. After relevant validation and a local commit, push the current branch to its configured upstream; do not open a pull request unless requested.

## Catch-up requests
- Read `docs/catch-up.md` before handling a user request phrased as "catch-up" or "катчап". It is the canonical definition of daily scope, output rules, media handling, and completion checks.
- A catch-up means the standard `main` profile daily catch-up through yesterday. Do not invent separate full-chat or full-account catch-up formats; `dump` and `sync` are low-level data primitives, not user-facing catch-up workflows.
- A successful `daily-catchup` must atomically publish `reports/daily/00-latest-catchup.md` from every daily report in that run's range. Treat individual `YYYY-MM-DD.md` files as the sources and the merged file as the handoff view.
- `daily-catchup` must collect all missing days through one sequential Telegram range scan, then partition records into day reports. Do not reintroduce one full dialog/chat scan per day.
- Daily dialog collection must use `messages.getHistory` and filter self/additional senders locally. Do not use `messages.search` as a completeness source: an empty-query self search reproducibly omitted a real outgoing message. A short history page alone is not a proven range boundary. Early completion is allowed only for a valid checkpoint-bounded first page whose exact response metadata, known head and exclusive `MinID` jointly prove the whole window; every other flow stops only at the date boundary, empty page or configured hard limit.
- The daily dialog checkpoint may skip history only for an automatic catch-up range contiguous with the last complete checkpoint, on the same Telegram account and identical daily scope, when the dialog `top_message_id` is unchanged and that head was fully covered by the previous range. Its safe `verified_message_id` comes from every raw history message actually read strictly before the exclusive range end, before sender/report filtering; incomplete scans never advance it. A head from the next unpublished day must be scanned from that boundary, never skipped. Explicit `--from`, gaps, historical ranges, state/account/scope mismatch, anomalous heads, incomplete scans, and errors must use the safe full-scan fallback. Publish the checkpoint only after the merged catch-up Markdown succeeds.
- `daily` and `daily-catchup` must directly measure Telegram scan, download, ffmpeg, model cold-start, backend-neutral ASR, and render, then atomically preserve a unique per-run JSON under `.state/daily/timings/`. Do not reconstruct historical stage timings from replaceable daily ASR logs.
- Daily media keeps one page-bounded producer and a global two-slot downloader. Consecutive small files may use one slot each; a large file uses both, FIFO order prevents starvation, and history RPC never overlaps a download wave. The bounded local queue feeds independent `ffmpeg → ASR` workers. Apply all results before deterministic sort/render; never let ASR workers own or call the Telegram client.
- `--asr-workers=auto` is the normal daily mode and its policy is backend-specific. Vosk CPU may grow from one to at most four only for proven queued benefit plus CPU/memory headroom. whisper.cpp Metal/Core ML stays at one GPU worker; fixed `1..4` values are diagnostic overrides.
- Transcript cache identity must include backend, model/quantization, accelerator, language and material decode settings. Publication must be atomic; in-flight media keys must be deduplicated, and temporary source/WAV/transcript files cleaned on success, failure, cancellation, and interruption.
- Keep one public adaptive ASR profile for Telegram, local files, and OBS. Preserve the exact short-message strategy behind tested routing, validate long-form as a complete RU/EN/edge policy, include every transcript-affecting threshold in cache identity, and update the OBS contract atomically when semantics change.

## Code Policy
- Prefer small, testable helpers for env loading, MTProto auth, runtime locks, and flood-wait handling.
- Keep JSONL output stable and source-rich: every record should include chat, message id, date, sender, text/media metadata, Telegram source pointer, and structured `fwd_from` origin metadata for forwarded messages.
- Treat `.toon` outputs as rebuildable agent views only; JSONL remains the canonical lossless dump.
- Treat `agent-view/README.md` as the first file agents should open. It points to `all-recent.md`, then chat/topic/day Markdown files so agents do not read raw JSONL by default.
- Keep `agent-view/.agent-view-state.json` private/generated; it tracks the processed JSONL byte offset for incremental updates and should not be edited by hand.
- When generated `agent-view` templates or manifest semantics change, bump `agentViewManifestVersion` and keep rebuild/noop/incremental tests aligned.
- Keep generated `agent-view/AGENTS.md` and `agent-view/README.md` aligned whenever changing the agent read path; they are the agent-facing navigation source of truth.
- For forum chats, preserve `topic` and `thread_top_message_id`; do not merge topic streams only by chat title.
- Main profile uses `TG_HARVEST_DAILY_*`. Study profile uses `TG_HARVEST_STUDY_*`. Do not add alternate env aliases.
- `TG_HARVEST_DAILY_ADDITIONAL_SENDERS` contains comma-separated `chat_id:sender_id` pairs. Additional senders must remain scoped to their configured chats; never include all incoming messages from those chats.
- CLI commands must receive `--profile main|study`; do not add command-based profile defaults or profile env fallbacks.
- `send-saved` must remain recipient-free and self-only. Never resolve a username, phone, chat, or user for delivery; never route through another account. Its `main` session must identify as `@Pheik13` before the first write, and the sent message must be verified by self-peer readback. For files, filename, MIME type, and byte size must all match.
- Telegram pacing/history defaults are code-owned; do not add env knobs for RPC spacing, history batch size, history limit, max batches, or dialog limit.
- Both profiles use explicit Telegram API credentials and CLI `login`; do not read or import Telegram Desktop `tdata`.
- Study `dump` and `sync` do not transcribe audio/video. They save inspectable study materials such as photos, image documents, and generic documents; audio/video transcription is a daily-harvest feature only.
- Daily audio/video media is transcript-only: cache by Telegram media id when possible, delete temporary source media after transcription, and keep saved `local_path` only for images/documents agents need to inspect.
- For pinned whisper.cpp behavior, verify the installed source execution path and a real current-head A/B; request-field unit tests alone do not prove that an upstream option is effective.
- Daily generic `video` ASR defaults to `--transcribe-video=phone`: only vertical phone-like videos with Telegram metadata, <=6 minutes, <=80 MiB, and no more than 1080x1920. Use explicit `--transcribe-video=all` only when the user asks to transcribe generic videos broadly.
- Default media caps are deliberate: photo/image and generic documents 10 MiB, audio/voice 50 MiB, round/video 200 MiB, plus the generic video ASR prefilter above. If a cap is exceeded, keep the skip reason and manual `download-media` hint in output.
- When changing CLI commands or flags, update CLI help, Makefile shortcuts, README/.env examples when relevant, and command tests in the same pass.
- Do not add backwards-compatibility command aliases, profile aliases, env aliases, shims, or fallback code paths unless the user explicitly requests them. If compatibility seems useful, raise it as a question first; otherwise remove the old path when replacing it.
- Add tests for parsing/config/state behavior; live Telegram behavior is validated manually after login.
