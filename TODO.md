# TODO

Deferred enhancements to implement later:

- Add a compact-session export / import flow independent of history.jsonl
- Add smarter summary generation for compaction that prioritizes changed files and pending work

Notes:
- Session compaction runs automatically once a turn's token usage passes `GOAT_AUTO_COMPACT_TOKENS` and is also available manually via `compact`.
- Keep preview-first workflows for mutating tools.
