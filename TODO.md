# TODO

Deferred enhancements to implement later:

- Add a compact-session export / import flow independent of history.jsonl
- Add smarter summary generation for compaction that prioritizes changed files and pending work

Notes:
- Session compaction runs automatically before oversized follow-up turns and is also available manually via `compact`.
- Keep preview-first workflows for mutating tools.
