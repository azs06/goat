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
  - A provider's *_BASE_URL is not settled together with its key, so a project .env that names a proxy without its own credential still gets the key from the shell or ~/.config/goat/.env (it was the same with shell keys before). Option: warn at startup when a provider's base URL and its key come from different places.
  - On Windows environment names are case-insensitive, but the .env loader's checks (HOME/XDG_CONFIG_HOME not settable, credential grouping) compare names case-sensitively, so `xdg_config_home=` or `anthropic_auth_token=` in a .env slips past them. Untested; needs a Windows run.

Notes:
- Session compaction runs automatically once a turn's token usage passes `GOAT_AUTO_COMPACT_TOKENS` and is also available manually via `compact`.
- Keep preview-first workflows for mutating tools.
