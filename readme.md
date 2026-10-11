# G.O.A.T

A coding agent for the terminal, written in Go as a way to learn how agent harnesses
work by building one. goat talks to OpenAI, Anthropic (Claude) and DeepSeek models, works
on the files of the directory it is started in, and can run shell commands with your
approval. From a certain point on, goat has been improving its own source code.

/btw This is an experimental project.

- Streams replies as formatted markdown, with syntax-highlighted code and clickable links.
- Reads, searches and edits files through its own tools, with a diff for every change and
  `/undo` for every file edit.
- Runs shell commands only after you approve them, without your API keys in their
  environment.
- Lets you keep typing while it works: queued messages steer the turn in progress.
- Picks models and effort with the arrow keys, keeps prompt history, completes paths, and
  takes multi-line and pasted input.
- Lays requests out for the providers' prompt caches and shows what each turn cost.
- Reads project instructions from `AGENTS.md`, and has a plan mode that changes nothing.
- Has a code mode: the model can write one JavaScript script that calls many tools, and
  only the script's result comes back into the conversation.
- Works the same in scripts: `goat -p "..."` prints just the reply.

## Quick start

goat needs Go 1.25 or newer and an API key for at least one provider:

```sh
export OPENAI_API_KEY=...      # OpenAI models, and/or
export ANTHROPIC_API_KEY=...   # Claude models (ANTHROPIC_AUTH_TOKEN also works), and/or
export DEEPSEEK_API_KEY=...    # DeepSeek models

go install github.com/azs06/goat@latest && goat
# or, from a checkout: go run .   (or go build -o goat . && ./goat)
```

Keys can also go in `~/.config/goat/.env` (`$XDG_CONFIG_HOME/goat/.env` when
`XDG_CONFIG_HOME` is set), which goat reads in every folder, so a `go install`ed goat
finds them wherever you start it:

```sh
mkdir -p ~/.config/goat
(umask 077 && touch ~/.config/goat/.env)   # readable only by you
${EDITOR:-vi} ~/.config/goat/.env           # add a line such as OPENAI_API_KEY=sk-...
```

Adding the key in an editor keeps it out of your shell history. The shell wins: a
variable it sets to a non-empty value keeps that value. A provider's key is settled as a
whole: if the shell supplies `ANTHROPIC_AUTH_TOKEN`, a global `ANTHROPIC_API_KEY` is not
used. goat does not read the `.env` in the project it works on: that file belongs to the
project (its own keys, database URLs and so on), and a cloned repository could otherwise
point a provider's base URL at a server that would receive your key. `/status` shows where
each provider's key came from, and goat warns if the global file can be read by other
users. If the file cannot be parsed, goat names it (and the line, where it can tell)
without printing its contents. Only providers with a key are offered. Run
goat from the root of the project you want it to work on: that directory is its
workspace.

## Using goat

Type a request and press Enter. goat streams the reply and shows each tool call as it
happens, with a short summary of the result and a diff for file edits; `/output` shows a
result in full. Press Ctrl-C while it is working to interrupt the turn; the next prompt
continues from where it stopped. Ctrl-C at an empty prompt asks you to press it again to
exit, and Ctrl-D exits straight away.

The prompt sits between two rules. The footer under it shows the model, the effort, the
git branch with the number of changed files, and the conversation's approximate size
(plus `plan mode` or `code mode` when they are on);
`?` on an empty prompt replaces it with the keyboard shortcuts.

### The prompt

| Key | |
| --- | --- |
| Enter | Send the prompt, or run the command selected in the menu |
| Ctrl-J, Alt-Enter, `\` then Enter | Start a new line |
| Up/Down, Ctrl-P/Ctrl-N | Move in a menu, move between the lines of a longer prompt, or bring back earlier prompts (kept across sessions) |
| Tab | Complete the selected command, or the file path before the cursor (also after `@`); several matches open a menu |
| Esc | Close the menu |
| Left/Right, Home/End, Ctrl-A/Ctrl-E | Move the cursor; Home and End stay on the current line |
| Alt-Left/Alt-Right, Alt-B/Alt-F | Move by word |
| Ctrl-U, Ctrl-K, Ctrl-W | Delete to the start of the line, to the end, or the word before the cursor |
| `?` | On an empty prompt, show these keys |

Typing `/` opens a menu of matching commands; after a command and a space it suggests the
command's arguments, such as the effort levels after `/effort `. Pasted text keeps its
line breaks and is never sent before you press Enter.

### Typing while goat works

The line under the spinner takes a message while a turn runs, and Enter queues it. Queued
text goes to the model with the next tool results, so it can change course mid-turn;
anything still queued when the turn ends is sent as the next prompt. Slash commands wait
until the turn is over. Up on an empty line takes the last queued message back to edit.
After Ctrl-C or a failed turn nothing queued is sent; it is put back in the prompt
instead. While the reply is streaming the line is hidden, but what you type is kept.
Queueing needs an interactive color terminal.

### Command line

```sh
goat                          # start, resuming the last conversation
goat "fix the failing test"   # start and run this prompt first
goat -n                       # start a new conversation (the old one stays for /resume)
goat --plan                   # start in plan mode: read and plan, change nothing
goat --codemode               # start in code mode: batch tool calls in run_code scripts
goat -m claude-sonnet-5-5 -e high
goat -p "summarize main.go"   # one prompt, reply on stdout, exit code 1 on failure
git diff | goat -p "review this diff" > review.md
goat -p -c "and the tests?"   # -p continuing the saved conversation
goat --version; goat --help
```

A `-p` run prints tool activity to stderr, so stdout holds only the reply. Unless `-c` is
given it uses a conversation of its own and leaves the saved session alone. `-y` lets
`run_bash` commands run without asking, like `GOAT_AUTO_APPROVE_BASH=1`.

goat also works without a terminal. Piped or redirected output is plain text with no
escape sequences, and each line of piped input is one prompt or command:

```sh
printf 'summarize main.go\n/exit\n' | goat > summary.txt
```

Nobody can approve `run_bash` commands in that mode, so they are declined unless `-y` or
`GOAT_AUTO_APPROVE_BASH=1` is given.

## Commands

| Command | |
| --- | --- |
| `/model [name]` | Pick a model with the arrow keys (type to filter), or `/model <name or number>`. The last entry, and `/model list`, ask each provider for every model. Only providers with an API key are listed. |
| `/effort [level]` | Pick the reasoning effort with the arrow keys, or set it directly: `none`, `minimal`, `low`, `medium`, `high`, `xhigh` or `max`, mapped to what the model supports. |
| `/status` | Model, effort, which providers are available, conversation and context size, and settings. |
| `/clear` (`/new`, `/reset`) | Start a new conversation; the old one is kept. |
| `/resume` | Swap back to the previous conversation; `/resume` again swaps back. |
| `/compact [focus]` | Summarize the conversation into a fresh, smaller context, optionally focusing on a topic. |
| `/retry` | Send the last prompt again. |
| `/output [n]` | Show a tool call's full output; tool results on screen only show the first lines. Without `n`, pick from the recent calls; `1` is the latest. Output taller than the terminal is paged: Space for the next page, Enter for a line, `G` for the rest, `q` to stop. |
| `/plan [on\|off]` | Plan mode: goat reads and investigates but changes nothing. File tools only run as previews and `run_bash` is refused; the footer shows `plan mode`. `goat --plan` starts in it. |
| `/codemode [on\|off]` | Code mode: the model can batch tool calls in one `run_code` script (see [Code mode](#code-mode)); the footer shows `code mode`. `goat --codemode` or `GOAT_CODEMODE=1` starts in it. |
| `/diff [staged]` | Show the uncommitted changes in the workspace's git repository, paged like `/output`. |
| `/copy` | Copy the last reply to the clipboard (pbcopy, wl-copy, xclip, xsel, or the terminal's OSC 52). |
| `/export [file]` | Write the conversation, tool calls and outputs included, to a Markdown file in the workspace. |
| `/undo [preview\|skip]` | Undo the latest file change. `preview` shows what would be restored; `skip` gives up on a change that cannot be restored. |
| `/approve [on\|off]` | Show or change whether `run_bash` runs commands without asking. |
| `/theme` | Reload the theme file and show which one is in use. |
| `/tools` | List the agent's tools. |
| `/history` | Show where the conversation history is written. |
| `/help` | List the commands and keys. |
| `/exit` (`/quit`) | Exit. |

The pickers (`/model`, `/effort`, `/output`) take Up/Down to move, typing to filter,
Enter to choose and Esc to cancel.

The older commands (`help`, `exit`, `quit`, `status`, `compact`, `undo`, `tools`, `history`,
`resume`, `reset`) also run when typed alone without the slash. Newer ones need the slash,
so a one-word prompt such as "clear" still goes to the agent.

## Tools

The model works on the workspace through these tools. All paths are relative to the
workspace.

| Tool | |
| --- | --- |
| `read_dir`, `tree_dir` | List a directory, or draw a recursive tree |
| `read_file` | Read a file, or a range of its lines with `offset` and `limit` (whole files over 1 MiB are refused) |
| `find_files`, `grep_files` | Find files by glob or substring; search file contents as text or a regular expression (`regex`), optionally only in files matching `glob` |
| `write_file`, `edit_file` | Write a file, or replace exact text in one |
| `edit_json_file` | Set or delete a value at a JSON path |
| `copy_file`, `move_file`, `delete_file` | File operations |
| `copy_dir`, `move_dir`, `delete_dir` | Directory operations |
| `undo_last_change` | Undo the latest file or directory change |
| `run_bash` | Run a shell command in the workspace, after you approve it |
| `run_code` | Run a JavaScript script that calls the other tools; only in code mode |

`find_files`, `grep_files` and `tree_dir` skip what `.gitignore` excludes (in a git
repository git decides; elsewhere the root `.gitignore` is read); `include_ignored=true`
searches everything. Every tool that changes files accepts `preview=true`, which returns a
diff without changing anything. A turn can make up to 40 rounds of tool calls; past that
goat stops and asks you to send another message to continue.

## Code mode

With code mode on (`/codemode`, `goat --codemode` or `GOAT_CODEMODE=1`), the model can
answer with one `run_code` call holding a JavaScript async function instead of a string of
separate tool calls. The script calls the tools as functions on a `goat` object, each
taking the tool's arguments as one object and resolving to the text that tool returns:

```js
async () => {
  const found = await goat.find_files({ path: "internal", pattern: "*_test.go" });
  const counts = [];
  for (const path of found.split("\n").slice(1).filter(line => !line.startsWith("(truncated"))) {
    const text = await goat.read_file({ path });
    counts.push({ path, tests: text.split("\n").filter(l => l.startsWith("func Test")).length });
  }
  return counts.sort((a, b) => b.tests - a.tests).slice(0, 3);
}
```

Only what the script returns and what it prints with `console.log` go back to the model,
so a loop over many files, a search followed by reads, or filtering a long output takes
one round trip and keeps the intermediate text out of the conversation. The calls the
script makes are shown indented under it as they run.

- Every call goes through the same checks as a direct call: workspace paths, the `.env`
  and `.goat` refusals, plan mode, and approval for each `run_bash` command. Each change is
  its own `/undo` step.
- The script runs in an embedded JavaScript engine ([goja](https://github.com/dop251/goja))
  with nothing but `goat` and `console`: no file, network, environment or process access,
  no `require`, no timers. It is plain JavaScript; TypeScript annotations are a syntax
  error.
- A script may make 50 calls and run for 2 minutes; time spent waiting for you to approve
  a command does not count, and each command keeps its own 30-second limit. Ctrl-C stops
  the script. Its result is cut to the same size as any other tool output. `JSON.parse`
  refuses input nested more than 5000 levels deep, and one regular expression match may
  take at most 2 seconds (one that runs out of time counts as no match).
- A failed call rejects with an error the script can catch. An uncaught error, or a failed
  call nothing awaited, ends the script, and the model gets the error, the line it came
  from and the console output; changes made before it are kept. Declining a command stops
  the script, so your answer goes straight to the model.

`run_code` and the description of its API (about 1,900 tokens) are sent only while code
mode is on; with it off, goat sends exactly what it would without code mode. Turning it on
or off mid-conversation is fine, but the next request misses the prompt cache once. Code
mode is off by default and, like plan mode, is not saved with the session.

Code mode pays off on tasks that take many tool calls and that the shell cannot do in one
command. A benchmark of four read-only tasks (gpt-6-luna and deepseek-v4-pro, three runs
of each, measured against goat with code mode off) found:

- Without the shell, it cut wall time by 40-52% and input tokens by about 70%. gpt-6-luna
  answered all 12 tasks correctly with it, against 9 of 12 without, because long tool
  output got cut off.
- With the shell, gpt-6-luna was 31% faster. deepseek-v4-pro was 48% slower and used 49%
  more input, because it often wrapped a single shell command in a script.
- For a single-step task, the extra prompt is pure cost: 1.5 to 1.7 times the input
  tokens.

## Cost

After each turn the "Worked for" line shows what the turn cost, and `/status` shows the
conversation's total, both worked out from the token usage the API reports, cache reads
and writes included. Prices are built in for the Claude models; for others, or to use
your own rates, add them in US dollars per million tokens to
`~/.config/goat/prices.json`:

```json
{"gpt-6-luna": {"input": 1.25, "output": 10, "cache_read": 0.125},
 "deepseek-v4-pro": {"input": 0.5, "output": 2, "cache_read": 0.05}}
```

A cost followed by `+` leaves out responses from models without a price.

## Project instructions

Put notes for the agent in `AGENTS.md` at the workspace root (`GOAT.md` or `CLAUDE.md`
are used when there is no `AGENTS.md`), and personal ones in `~/.config/goat/AGENTS.md`.
They are sent at the start of every new conversation, including one that starts from a
`/compact` summary, together with the git branch and the list of uncommitted changes when
the workspace is a repository. They go with the first message rather than into the system
prompt, so the cached prompt prefix stays the same and editing them costs no cache.
`/status` shows which files are in use.

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
| `prompts.jsonl` | The prompts typed at the interactive prompt, for Up |
| `operations.json`, `snapshots/` | The record of file changes and the snapshots `/undo` restores from |

When a turn's token usage passes `GOAT_AUTO_COMPACT_TOKENS`, goat compacts the
conversation automatically, the same way `/compact` does: it is summarized into a fresh,
smaller context. If `session.json` or `operations.json` cannot be read, goat moves it
aside with a `.corrupt-<time>` suffix, warns, and starts fresh.

## Safety

- File tools only accept paths inside the workspace, refuse symlinks that lead outside it,
  and cannot touch `.goat/`. They also refuse `.env`, `.env.*` and `.envrc` (templates such
  as `.env.example` stay readable), and `grep_files` skips them, so credentials never reach
  the model through a file tool.
- A provider's key goes to its own API unless its `*_BASE_URL` says otherwise, and only
  your shell or `~/.config/goat/.env` can say so. goat refuses a base URL that is not
  http(s) or that carries a user name or password. For any other host it says at startup,
  and in `/status`, where the requests go and which file set the URL, flagging plain http
  to another machine as unencrypted (http to `localhost` is fine). It shows only the
  scheme and host, since some proxies take a token in the path.
- `run_bash` commands do not inherit goat's API keys, anything loaded from
  `~/.config/goat/.env`, or other variables named like credentials (`*_API_KEY`, `*_SECRET`,
  `*_AUTH_TOKEN`, `*_PASSWORD` and similar). `~/.config/goat/.env` is outside the
  workspace, so file tools cannot reach it, but a command can read it like any other file
  you can read: approving commands is what guards it.
- Every change made by a file tool is snapshotted first, so `/undo` can restore it,
  including removing directories the change created. The last 100 operations stay
  undoable. If one cannot be restored, `/undo skip` gives up on it so earlier ones can
  still be undone.
- In plan mode (`/plan`, `--plan`) nothing in the workspace changes: file tools only run
  as previews and `run_bash` is refused.
- A `run_code` script can only call goat's own tools, each with the checks above; the
  engine it runs in gives it no other access to files, the network, the environment or
  processes.
- `run_bash` asks before each command. Answer `y` to run it, `n` to decline, `a` to allow
  every command for the rest of the session, or type feedback to decline with a reason the
  model sees. Commands time out after 30 seconds, and everything a command started is
  killed with it. Changes made through `run_bash` are not snapshotted. A command that
  mentions `.goat` is always confirmed, even when commands otherwise run without asking.

## Themes

goat's look is black and white by default; the one exception is syntax highlighting in
code blocks, which uses color. All styling goes through a small style engine
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
  `heading`, `subheading`, `quote`, `code`, `code-comment`, `code-keyword`, `code-string`,
  `code-number`, `code-function`, `code-type`, `code-constant`, `inline-code`, `success`,
  `error`, `warning`, `approval-title`, `diff-added`, `diff-removed`, `prompt`,
  `user-marker`, `user-message` and `selected`. A role can also be written like a class
  (`".accent"`).

Mistakes are reported as warnings at startup, and only the broken declaration is skipped.
A file that is not valid JSON leaves the built-in theme in place. Themes never turn
styling on where it is off: piped output, `NO_COLOR` and `TERM=dumb` stay plain.

## Configuration

Any of these can also be set in `~/.config/goat/.env`, so it doubles as
goat's global settings file (e.g. `GOAT_CODEMODE=1`). `HOME` and `XDG_CONFIG_HOME` are
the exception: they locate the global file, so they come only from the environment.

| Variable | Default | Purpose |
| --- | --- | --- |
| `OPENAI_API_KEY` | | Enables OpenAI models |
| `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` | | Enables Claude models |
| `DEEPSEEK_API_KEY` | | Enables DeepSeek models |
| `OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL` | | Point a provider at a proxy or a compatible API (see [Safety](#safety) for the checks) |
| `DEEPSEEK_BASE_URL` | `https://api.deepseek.com/anthropic` | DeepSeek's Anthropic-compatible endpoint |
| `GOAT_MODEL` | | Model to start with, overriding the saved one (e.g. `claude-opus-5-5`) |
| `OPENAI_MODEL` | `gpt-6-luna` | OpenAI model when there is no saved session |
| `GOAT_EFFORT` | | Effort to start with, overriding the saved one |
| `OPENAI_REASONING_EFFORT` | `medium` | Effort when there is no saved session |
| `GOAT_AUTO_COMPACT_TOKENS` | `80000` (OpenAI), `150000` (Claude, DeepSeek) | Conversation size that triggers automatic compaction; lower it for small tokens-per-minute limits |
| `GOAT_AUTO_APPROVE_BASH` | `false` | Run `run_bash` commands without asking |
| `GOAT_CODEMODE` | `false` | Start in code mode |
| `GOAT_ANTHROPIC_FALLBACKS` | `on` | Server-side refusal fallbacks for Claude models |
| `GOAT_ANTHROPIC_CACHE_TTL` | `5m` | Lifetime of Claude prompt cache entries, `5m` or `1h` |
| `GOAT_THEME` | `~/.config/goat/theme.json` | Theme file |
| `GOAT_REDUCED_MOTION` | | Keep the spinner still (it updates once a second) |
| `NO_COLOR` | | Turn styling off |
| `GOAT_FORCE_COLOR` | | Style output even when stdout is not a terminal, for example when piping through a pager that understands colors |

goat also reads these files, all optional (`~/.config` is `$XDG_CONFIG_HOME` when set):

| File | |
| --- | --- |
| `~/.config/goat/.env` | API keys and other variables, loaded at startup in every folder; the shell wins. The workspace's `.env` is never read |
| `AGENTS.md` (or `GOAT.md`, `CLAUDE.md`) in the workspace | Project instructions |
| `~/.config/goat/AGENTS.md` | Your own instructions for every project |
| `~/.config/goat/theme.json` | Theme |
| `~/.config/goat/prices.json` | Model prices for cost reporting |

## Project layout

| Path | |
| --- | --- |
| `main.go`, `cli.go`, `env_files.go` | Startup, command-line flags, loading the global `.env`, the prompt loop and Ctrl-C handling |
| `agent.go` | One turn: streaming the reply, running tool calls, the 40-round limit |
| `commands.go` | Slash commands |
| `session.go`, `history.go`, `compaction.go` | Saved sessions, the history log and compaction |
| `system_prompt.go`, `project_context.go` | The system prompt, built from the tool list, and the project instructions |
| `plan_mode.go`, `code_mode.go` | Plan mode; code mode and drawing a script's calls |
| `prompt_editor.go`, `selector.go`, `input.go`, `queue.go` | The interactive line editor, arrow-key pickers, messages queued during a turn and input reading |
| `output.go`, `git.go`, `pricing.go` | `/output`, `/copy`, `/export`, `/diff`, the built-in pager, git status and cost |
| `render_*.go`, `spinner.go`, `messages.go`, `theme.go` | Drawing the transcript, the spinner, status messages and theme loading |
| `internal/codemode/` | The JavaScript sandbox `run_code` scripts run in: limits, console capture and the generated `goat` API declaration |
| `internal/markdown/` | Rendering streamed markdown replies, with syntax-highlighted code blocks |
| `internal/provider/` | The OpenAI, Anthropic and DeepSeek clients behind one interface, and the model catalog |
| `internal/style/` | The style engine: roles, CSS parsing, colors and themes |
| `internal/terminal/` | Terminal modes (echo, cbreak) and size |
| `internal/tools/` | The tools offered to the model: schemas, argument checks, plan-mode rules, the handler that runs each one, and `run_code` |
| `internal/workspace/` | The file tools: path resolution confined to the workspace, reads, `.gitignore`-aware search, previews, `run_bash`, and mutations snapshotted for undo; keeping credentials out of tools |

## Development

```sh
gofmt -l .                      # should print nothing
go vet ./...
staticcheck ./...               # configured in staticcheck.conf
go test -race ./...
go test -run '^$' -fuzz FuzzResolveToolPath -fuzztime 30s ./internal/workspace/
```

The tests run against fake provider servers, so they need no API keys. Fuzz targets cover
path resolution (`FuzzResolveToolPath`, in `internal/workspace`), markdown rendering
(`FuzzRenderMarkdownLine`, in `internal/markdown`), the prompt editor
(`FuzzPromptEditorKeys`) and theme parsing (`FuzzParseTheme`, in `internal/style`). CI (`.github/workflows/ci.yml`) runs all of the above on Linux and
macOS, with a 70% coverage floor and a cross-build for every platform.

When you change anything that prints, also check that piped output stays free of escape
sequences; this should print `0`:

```sh
printf '/status\n/exit\n' | go run . | grep -c $'\x1b'
```

See [`TODO.md`](TODO.md) for planned work.

goat is released under the [MIT License](LICENSE).
