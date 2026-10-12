# TODO

Deferred enhancements to implement later:

- Add a compact-session export / import flow independent of history.jsonl
- Add smarter summary generation for compaction that prioritizes changed files and pending work
- Code mode follow-ups:
  - Record the calls a run_code script makes in history, so `/output` can show each one in full (today only the script and its result are recorded).
  - One undo step for everything a script changed.
  - A code-only mode that offers just run_code (the tool list already depends on the mode, and both fingerprints count as current on resume).
  - Smoke-test turning code mode on or off mid-conversation with a Claude model: the replayed thinking blocks then sit under a different system prompt and tool list. OpenAI and DeepSeek accept it (tried live); if Claude refuses, goat's existing recovery compacts the conversation and retries.
  - Anthropic's server-side programmatic tool calling for Claude models (code_execution with allowed_callers); needs pause_turn handling, a container ID in the session, and its ~4 minute limit on a pending call fits badly with run_bash approval.
  - Arrays nested about a million deep, built in a script, can still overflow the Go stack through join/toString and kill goat; only running scripts in a subprocess would contain that.
  - After a script is abandoned in native code, the next run's heap watchdog takes its baseline while the old one may still be allocating, so it could be stopped for memory it did not use.
  - Tool text reaches scripts with its headers ("File contents for x:"); a raw-content option for scripts would save models from parsing them (one live run miscounted lines because of the header).
  - Smoke-test Claude models writing scripts (only OpenAI gpt-6-luna and DeepSeek v4 pro were tried against the real APIs).

- Global .env follow-ups:
  - Key commands run once at startup. A short-lived token that expires mid-session is not fetched again; re-running the command after a 401 would cover it.
  - providers.json entries: smoke-test against Ollama, LM Studio and vLLM (only DeepSeek's Chat Completions and Messages endpoints were tried live). OpenAI's own models refuse tools over Chat Completions while reasoning is on, so OpenAI entries suit other servers, not api.openai.com.
  - providers.json entries: an opt-in effort field for servers that accept `reasoning_effort`. Reasoning (`reasoning_content`, `reasoning`) is replayed, but per-call extras such as Gemini's `extra_content` thought signatures are not.
  - Run on Windows: the case-insensitive variable names and key commands through cmd.exe (including quoted arguments) have only been cross-built, never run. Or add windows-latest to the CI matrix.
  - Considered and rejected: hiding plain `*_TOKEN` variables such as `GH_TOKEN` from run_bash. `gh` and similar tools read them, so the agent's `gh` commands would stop working.

Notes:
- Session compaction runs automatically, before a prompt or between tool rounds, once the conversation passes `GOAT_AUTO_COMPACT_TOKENS`, and is also available manually via `compact`.
- Keep preview-first workflows for mutating tools.
