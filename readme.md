# An attem to learn about agent harness, by developing one in Go

/btw This is an experimental project

## Running

```sh
export OPENAI_API_KEY=...      # for OpenAI models, and/or
export ANTHROPIC_API_KEY=...   # for Claude models (ANTHROPIC_AUTH_TOKEN also works), and/or
export DEEPSEEK_API_KEY=...    # for DeepSeek models
go run .
```

Keys can also go in a `.env` file. Only providers with a key are offered.

Type a prompt to talk to the agent. Press Ctrl-C while it is working to interrupt the
current turn; the next prompt continues from where it stopped.

## Commands

| Command | |
| --- | --- |
| `/model` | Pick a model from a numbered list, or `/model <name or number>`; `/model list` also asks each provider for its models. Only providers with an API key are listed. |
| `/effort [level]` | Show or set the reasoning effort: `none`, `minimal`, `low`, `medium`, `high`, `xhigh` or `max` (mapped to what the model supports). |
| `/status` | Model, effort, which providers are available, conversation and context size. |
| `/clear` (`/new`, `/reset`) | Start a new conversation; `/resume` brings the previous one back. |
| `/compact [focus]` | Summarize the conversation into a fresh, smaller context. |
| `/retry` | Send the last prompt again. |
| `/undo [preview\|skip]` | Undo the latest file change. |
| `/approve [on\|off]` | Whether `run_bash` asks before each command. |
| `/tools`, `/history`, `/help`, `/exit` (`/quit`) | |

Commands start with `/`. The older ones (`help`, `exit`, `quit`, `status`, `compact`, `undo`,
`tools`, `history`, `resume`, `reset`) also run when typed alone, so a one-word prompt such
as "clear" still goes to the agent.

## Models and providers

- Names starting with `claude-` use Anthropic, `deepseek-` use DeepSeek, and `gpt-`,
  `o1`/`o3`/`o4`, `codex` and `chatgpt-` use OpenAI. Any other name needs a provider prefix,
  such as `openai:my-fine-tune` or `anthropic:claude-custom`.
- Switching models within a provider keeps the conversation. Switching to the other provider
  carries the conversation over as a summary (the full one stays available through `/resume`).
- The model, effort and conversation are saved in `.goat/session.json` and restored on start.
- Claude models run with adaptive thinking, prompt caching, and (on Claude Opus 5, Claude Opus
  5.5 and Claude Sonnet 5.5) server-side refusal fallbacks, which re-run a request a safety
  classifier declines on another Claude model. Set `GOAT_ANTHROPIC_FALLBACKS=off` to turn
  fallbacks off.
- DeepSeek (`deepseek-v4-pro`, `deepseek-flash`) is reached through DeepSeek's
  [Anthropic-compatible API](https://api-docs.deepseek.com/guides/anthropic_api), with thinking
  on and its reasoning replayed every turn as DeepSeek requires. `/effort none` turns thinking
  off; other levels map to DeepSeek's `low`, `high` and `max`. Anthropic credentials are never
  sent to DeepSeek.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `OPENAI_API_KEY` | | Enables OpenAI models |
| `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` | | Enables Claude models |
| `DEEPSEEK_API_KEY` | | Enables DeepSeek models |
| `DEEPSEEK_BASE_URL` | `https://api.deepseek.com/anthropic` | DeepSeek's Anthropic-compatible endpoint |
| `GOAT_MODEL` | | Model to start with, overriding the saved one (e.g. `claude-opus-5-5`) |
| `OPENAI_MODEL` | `gpt-6-luna` | OpenAI model when there is no saved session |
| `GOAT_EFFORT` | | Effort to start with, overriding the saved one |
| `OPENAI_REASONING_EFFORT` | `medium` | Effort when there is no saved session |
| `GOAT_AUTO_COMPACT_TOKENS` | `80000` (OpenAI), `150000` (Claude, DeepSeek) | Conversation size that triggers automatic compaction; lower it for small tokens-per-minute limits |
| `GOAT_AUTO_APPROVE_BASH` | `false` | Run `run_bash` commands without asking first |
| `GOAT_ANTHROPIC_FALLBACKS` | `on` | Server-side refusal fallbacks for Claude models |
| `OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL` | | Point a provider at a proxy or compatible API |

## Safety

- File tools only accept paths inside the workspace, refuse symlinks that lead outside
  it, and cannot touch `.goat/`, where history, the operation log and undo snapshots live.
- Every file tool change is snapshotted first, so `undo` can restore it, including
  removing directories the change created. The last 100 operations stay undoable. If one
  cannot be restored, `/undo skip` gives up on it so earlier ones can still be undone.
- `run_bash` asks before each command (answer `a` to allow all commands for the session),
  times out after 30 seconds, and kills everything the command started when it does.
  Changes made through `run_bash` are not snapshotted.
