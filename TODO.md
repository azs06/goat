# TODO

Deferred enhancements to implement later:

- Add a compact-session export / import flow independent of history.jsonl
- Add smarter summary generation for compaction that prioritizes changed files and pending work
- Code mode follow-ups:
  - Record the calls a run_code script makes in history, so `/output` can show each one in full (today only the script and its result are recorded).
  - One undo step for everything a script changed.
  - A code-only mode that offers just run_code; it changes the tool list, so it must be fixed per conversation (the prompt fingerprint covers it).
  - Anthropic's server-side programmatic tool calling for Claude models (code_execution with allowed_callers); needs pause_turn handling, a container ID in the session, and its ~4 minute limit on a pending call fits badly with run_bash approval.
  - Smoke-test Claude models writing scripts (only OpenAI gpt-6-luna and DeepSeek v4 pro were tried against the real APIs).

Notes:
- Session compaction runs automatically once a turn's token usage passes `GOAT_AUTO_COMPACT_TOKENS` and is also available manually via `compact`.
- Keep preview-first workflows for mutating tools.
