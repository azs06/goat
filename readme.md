# G.O.A.T

A coding agent for the terminal, written in Go as a way to learn how agent harnesses
work by building one. goat talks to OpenAI, Anthropic (Claude) and DeepSeek models, works
on the files of the directory it is started in, and can run shell commands with your
approval. From a certain point on, goat has been improving its own source code.

/btw This is an experimental project.

## Quick start

goat needs Go 1.25 or newer and an API key for at least one provider:

```sh
export OPENAI_API_KEY=...      # OpenAI models, and/or
export ANTHROPIC_API_KEY=...   # Claude models (ANTHROPIC_AUTH_TOKEN also works), and/or
export DEEPSEEK_API_KEY=...    # DeepSeek models

go run .                       # or: go build -o goat . && ./goat
```

Keys can also go in a `.env` file in the directory goat runs in. Only providers with a key
are offered. Run goat from the root of the project you want it to work on: that directory
is its workspace.

## Using goat

Type a prompt and press Enter. goat streams the reply as formatted markdown and shows
each tool call as it happens, with a short summary of the result and a diff for file
edits. Press Ctrl-C while it is working to interrupt the turn; the next prompt continues
from where it stopped. Ctrl-C at an empty prompt asks you to press it again to exit, and
Ctrl-D exits straight away.

You can keep typing while goat works: the line under the spinner takes a message, and
Enter queues it. Queued text goes to the model with the next tool results, so it can
change course mid-turn; anything still queued when the turn ends is sent as the next
prompt. Slash commands wait until the turn is over. Up on an empty line takes the last
queued message back to edit. After Ctrl-C or a failed turn nothing queued is sent; it is
put back in the prompt instead. While the reply is streaming the line is hidden, but what you type is
kept. Queueing needs an interactive color terminal.

The prompt sits between two rules, with a footer showing the model, the effort and the
conversation's approximate size. Typing `/` opens a menu of matching commands in place of
the footer:

| Key | |
| --- | --- |
| Up/Down, Ctrl-P/Ctrl-N | Move the menu selection |
| Tab | Complete the selected command so you can add arguments |
| Enter | Run the selected command, or send the prompt |
| Esc | Close the menu |
| Left/Right, Home/End, Ctrl-A/Ctrl-E | Move the cursor |
| Ctrl-U, Ctrl-K, Ctrl-W | Delete to the start of the line, to the end, or the word before the cursor |

goat also works without a terminal. Piped or redirected output is plain text with no
escape sequences, and each line of piped input is one prompt or command:

```sh
printf 'summarize main.go\n/exit\n' | goat > summary.txt
```

Nobody can approve `run_bash` commands in that mode, so they are declined unless
`GOAT_AUTO_APPROVE_BASH=1` is set.

## Commands

| Command | |
| --- | --- |
| `/model` | Pick a model from a numbered list, or `/model <name or number>`. `/model list` also asks each provider for its models. Only providers with an API key are listed. |
| `/effort [level]` | Show or set the reasoning effort: `none`, `minimal`, `low`, `medium`, `high`, `xhigh` or `max`, mapped to what the model supports. |
| `/status` | Model, effort, which providers are available, conversation and context size, and settings. |
| `/clear` (`/new`, `/reset`) | Start a new conversation; the old one is kept. |
| `/resume` | Swap back to the previous conversation; `/resume` again swaps back. |
| `/compact [focus]` | Summarize the conversation into a fresh, smaller context, optionally focusing on a topic. |
| `/retry` | Send the last prompt again. |
| `/undo [preview\|skip]` | Undo the latest file change. `preview` shows what would be restored; `skip` gives up on a change that cannot be restored. |
| `/approve [on\|off]` | Show or change whether `run_bash` runs commands without asking. |
| `/theme` | Reload the theme file and show which one is in use. |
| `/tools` | List the agent's tools. |
| `/history` | Show where the conversation history is written. |
| `/help` | List the commands. |
| `/exit` (`/quit`) | Exit. |

The older commands (`help`, `exit`, `quit`, `status`, `compact`, `undo`, `tools`, `history`,
`resume`, `reset`) also run when typed alone without the slash. Newer ones need the slash,
so a one-word prompt such as "clear" still goes to the agent.

## Tools

The model works on the workspace through these tools. All paths are relative to the
workspace.

| Tool | |
| --- | --- |
| `read_dir`, `tree_dir` | List a directory, or draw a recursive tree |
| `read_file` | Read a file (files over 1 MiB are refused) |
| `find_files`, `grep_files` | Find files by glob or substring; search file contents |
| `write_file`, `edit_file` | Write a file, or replace exact text in one |
| `edit_json_file` | Set or delete a value at a JSON path |
| `copy_file`, `move_file`, `delete_file` | File operations |
| `copy_dir`, `move_dir`, `delete_dir` | Directory operations |
| `undo_last_change` | Undo the latest file or directory change |
| `run_bash` | Run a shell command in the workspace, after you approve it |

Every tool that changes files accepts `preview=true`, which returns a diff without changing
anything. A turn can make up to 40 rounds of tool calls; past that goat stops and asks you
to send another message to continue.

## Models and providers

| Provider | Models offered | Default |
| --- | --- | --- |
| OpenAI | `gpt-6-luna`, or `OPENAI_MODEL` | `gpt-6-luna` |
| Anthropic | `claude-opus-5-5`, `claude-fable-5-1`, `claude-sonnet-5-5`, `claude-haiku-5-5` | `claude-opus-5-5` |
| DeepSeek | `deepseek-v4-pro`, `deepseek-flash` | `deepseek-v4-pro` |

- Model names starting with `claude-` use Anthropic, `deepseek-` use DeepSeek, and `gpt-`,
  `o1`/`o3`/`o4`, `codex`, `chatgpt-` and `ft:` use OpenAI. Any other name needs a provider
  prefix, such as `openai:my-fine-tune` or `anthropic:claude-custom`.
- The `/model` picker, including `/model list`, offers only OpenAI's GPT-5.6 and GPT-6
  models, Claude's 5.5 models and Claude Fable 5.1, and DeepSeek's newest pro and flash
  models. The model in use is always listed, and any other model can still be picked by
  name, e.g. `/model gpt-5.5`.
- Switching models within a provider keeps the conversation. Switching to another provider
  carries it over as a summary; the full conversation stays available through `/resume`.
- Claude models run with adaptive thinking, prompt caching and, on Claude Opus 5, Claude
  Opus 5.5 and Claude Sonnet 5.5, server-side refusal fallbacks. A fallback re-runs a
  request that a safety classifier declines on another Claude model. Set
  `GOAT_ANTHROPIC_FALLBACKS=off` to turn fallbacks off.
- Requests are laid out for the providers' prompt caches, which bill cached input at a
  fraction of the normal price: the system prompt and tools never change within a run and
  conversations are only ever appended to, so each request repeats the previous one as
  its prefix. Claude gets cache breakpoints after the tools and system prompt and at the
  end of the transcript (5-minute entries; `GOAT_ANTHROPIC_CACHE_TTL=1h` keeps them
  across longer pauses at twice the write price). OpenAI requests carry a
  `prompt_cache_key` and ask for 24-hour cache retention; DeepSeek caches prefixes on its
  own. Switching models, or changing `/effort` on Claude, starts the cache over. `/status`
  shows how much of the conversation's input was read from cache.
- DeepSeek is reached through its
  [Anthropic-compatible API](https://api-docs.deepseek.com/guides/anthropic_api), with
  thinking on and its reasoning replayed every turn, as DeepSeek requires. `/effort none`
  turns thinking off; other levels map to DeepSeek's `low`, `high` and `max`. Anthropic
  credentials are never sent to DeepSeek.

## Conversations and state

goat keeps its state in `.goat/` inside the workspace:

| File | |
| --- | --- |
| `session.json` | The model, the effort and the current conversation, restored on the next start |
| `session.prev.json` | The previous conversation, for `/resume` |
| `history.jsonl` | An append-only log of every prompt, reply and tool call |
| `operations.json`, `snapshots/` | The record of file changes and the snapshots `/undo` restores from |

When a turn's token usage passes `GOAT_AUTO_COMPACT_TOKENS`, goat compacts the
conversation automatically, the same way `/compact` does: it is summarized into a fresh,
smaller context. If `session.json` or `operations.json` cannot be read, goat moves it
aside with a `.corrupt-<time>` suffix, warns, and starts fresh.

## Safety

- File tools only accept paths inside the workspace, refuse symlinks that lead outside it,
  and cannot touch `.goat/`.
- Every change made by a file tool is snapshotted first, so `/undo` can restore it,
  including removing directories the change created. The last 100 operations stay
  undoable. If one cannot be restored, `/undo skip` gives up on it so earlier ones can
  still be undone.
- `run_bash` asks before each command. Answer `y` to run it, `n` to decline, `a` to allow
  every command for the rest of the session, or type feedback to decline with a reason the
  model sees. Commands time out after 30 seconds, and everything a command started is
  killed with it. Changes made through `run_bash` are not snapshotted.

## Themes

goat's look is black and white by default. All styling goes through a small style engine
(`internal/style`): text is styled by role, and a theme decides what each role looks like.
A theme is a JSON file whose styles are CSS declarations, written as a string or as an
object:

```json
{
  "styles": {
    ":root": "--brand: #d7875f",
    "accent": "color: var(--brand)",
    "heading": {"color": "var(--brand)", "font-weight": "bold"},
    "diff-added": "color: palegreen",
    "diff-removed": "color: salmon; text-decoration: line-through"
  }
}
```

goat reads the file `GOAT_THEME` names, or else `~/.config/goat/theme.json`
(`$XDG_CONFIG_HOME/goat/theme.json` when that is set). It is layered over the built-in
theme, [`internal/style/default_theme.json`](internal/style/default_theme.json), property
by property, the way CSS cascades: roles you leave out keep their defaults, and
`font-weight: normal` turns a default bold off. `/theme` reloads the file.

- **Properties:** `color`, `background-color` (or `background`), `font-weight` (`bold`,
  `normal`, `lighter`, or 100-900; light weights draw faint), `font-style` (`italic`,
  `normal`) and `text-decoration` (`underline`, `line-through`, `none`).
- **Colors:** CSS named colors, `#rgb`, `#rrggbb`, `rgb()`, and `none` or `inherit` for the
  terminal's default. `ansi(n)` picks entry n of the terminal's own 256-color palette.
  Colors are sent as 24-bit color when `COLORTERM` is `truecolor` or `24bit`, and as the
  nearest 256-color entry otherwise.
- **Variables:** custom properties go in `":root"` and are used with `var(--name)` or
  `var(--name, fallback)`.
- **Roles:** `accent`, `muted`, `subtle`, `strong`, `emphasis`, `highlight`, `title`,
  `heading`, `subheading`, `quote`, `code`, `code-comment`, `inline-code`, `success`,
  `error`, `warning`, `approval-title`, `diff-added`, `diff-removed`, `prompt`,
  `user-marker`, `user-message` and `selected`. A role can also be written like a class
  (`".accent"`).

Mistakes are reported as warnings at startup, and only the broken declaration is skipped.
A file that is not valid JSON leaves the built-in theme in place. Themes never turn
styling on where it is off: piped output, `NO_COLOR` and `TERM=dumb` stay plain.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `OPENAI_API_KEY` | | Enables OpenAI models |
| `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` | | Enables Claude models |
| `DEEPSEEK_API_KEY` | | Enables DeepSeek models |
| `OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL` | | Point a provider at a proxy or a compatible API |
| `DEEPSEEK_BASE_URL` | `https://api.deepseek.com/anthropic` | DeepSeek's Anthropic-compatible endpoint |
| `GOAT_MODEL` | | Model to start with, overriding the saved one (e.g. `claude-opus-5-5`) |
| `OPENAI_MODEL` | `gpt-6-luna` | OpenAI model when there is no saved session |
| `GOAT_EFFORT` | | Effort to start with, overriding the saved one |
| `OPENAI_REASONING_EFFORT` | `medium` | Effort when there is no saved session |
| `GOAT_AUTO_COMPACT_TOKENS` | `80000` (OpenAI), `150000` (Claude, DeepSeek) | Conversation size that triggers automatic compaction; lower it for small tokens-per-minute limits |
| `GOAT_AUTO_APPROVE_BASH` | `false` | Run `run_bash` commands without asking |
| `GOAT_ANTHROPIC_FALLBACKS` | `on` | Server-side refusal fallbacks for Claude models |
| `GOAT_ANTHROPIC_CACHE_TTL` | `5m` | Lifetime of Claude prompt cache entries, `5m` or `1h` |
| `GOAT_THEME` | `~/.config/goat/theme.json` | Theme file |
| `NO_COLOR` | | Turn styling off |
| `GOAT_FORCE_COLOR` | | Style output even when stdout is not a terminal, for example when piping through a pager that understands colors |

## Project layout

| Path | |
| --- | --- |
| `main.go` | Startup, the prompt loop and Ctrl-C handling |
| `agent.go` | One turn: streaming the reply, running tool calls, the 40-round limit |
| `commands.go` | Slash commands |
| `tool_definitions.go`, `tools.go`, `json_tools.go` | Tool schemas and implementations |
| `mutations.go` | Snapshots, the operation log and undo |
| `session.go`, `history.go`, `compaction.go` | Saved sessions, the history log and compaction |
| `system_prompt.go` | The system prompt, built from the tool list |
| `prompt_editor.go`, `input.go`, `queue.go`, `terminal_guard_*.go` | The interactive line editor, messages queued during a turn, input reading and terminal modes |
| `render_*.go`, `spinner.go`, `messages.go`, `theme.go` | Drawing the transcript, the spinner, status messages and theme loading |
| `internal/provider/` | The OpenAI, Anthropic and DeepSeek clients behind one interface, and the model catalog |
| `internal/style/` | The style engine: roles, CSS parsing, colors and themes |

## Development

```sh
go vet ./...
go test ./...
```

The tests run against fake provider servers, so they need no API keys. When you change
anything that prints, also check that piped output stays free of escape sequences; this
should print `0`:

```sh
printf '/status\n/exit\n' | go run . | grep -c $'\x1b'
```

See [`TODO.md`](TODO.md) for planned work. Not built yet: prompt history (Up/Down only
move the command menu), multi-line input and bracketed paste.
