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

## Setup

### 1. Install

goat needs [Go](https://go.dev/dl/) 1.25 or newer.

```sh
go install github.com/azs06/goat@latest
goat --version
```

`go install` puts the binary in `$(go env GOPATH)/bin` (usually `~/go/bin`). If the shell
says `goat: command not found`, add that directory to your `PATH`, for example in
`~/.zshrc` or `~/.bashrc`:

```sh
export PATH="$PATH:$(go env GOPATH)/bin"
```

To build from a checkout instead: `git clone https://github.com/azs06/goat && cd goat &&
go install .` (or `go run .` to try it without installing).

### 2. Add an API key

goat needs a key for at least one provider: OpenAI for OpenAI models, Anthropic for
Claude models, DeepSeek for DeepSeek models, or one of the
[other providers](#other-providers) it knows, such as OpenRouter or Gemini. Only
providers with a key are offered (and the keyless local servers you
[add yourself](#optional-add-other-providers)).

The quickest way is to start goat and type `/login`: pick the provider and paste the
key. OpenAI, Anthropic, OpenRouter, xAI and Meta also list signing in with your account
(ChatGPT Plus/Pro, Claude Pro/Max, OpenRouter, Grok or X, Meta Muse), which is not
available yet,
so choose **API key** there; the other providers take only a key. goat
does not show the key as you type it, saves it to `~/.config/goat/auth.json` (readable
only by you), checks it by listing the provider's models, and starts using it at once.
`/login openai` skips the list. Without a key goat still starts at a terminal, so
`/login` is there on the first run; a saved conversation waits, untouched, until a key
gives goat a model. It is then set aside for `/resume` as at any start, or continued if
goat was started with `-c`.

Or put the keys in `~/.config/goat/auth.json` (`$XDG_CONFIG_HOME/goat/auth.json` when
`XDG_CONFIG_HOME` is set). goat reads it whichever folder it starts in, and keeps the
keys in its own memory, out of the environment of every command it runs:

```sh
mkdir -p ~/.config/goat
(umask 077 && touch ~/.config/goat/auth.json)   # readable only by you
${EDITOR:-vi} ~/.config/goat/auth.json
```

```json
{
  "openai": {"key": "sk-..."},
  "anthropic": {"key": "sk-ant-..."},
  "deepseek": {"key": "sk-..."}
}
```

`"openai": "sk-..."` is short for `"openai": {"key": "sk-..."}`. For Anthropic,
`{"auth_token": "..."}` sends a bearer token instead of an API key. Typing the key in an
editor keeps it out of your shell history. goat warns at startup if other users can read
the file (fix it with `chmod 600 ~/.config/goat/auth.json`), and its errors name a line
of the file but never print what is on it.

Keys exported in the shell work too (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY` or
`ANTHROPIC_AUTH_TOKEN`, `DEEPSEEK_API_KEY`), and win over the file:

```sh
export ANTHROPIC_API_KEY=sk-ant-...
```

Keys stay in `auth.json`, apart from goat's settings in
[`config.json`](#configuration), so the settings can be shared or kept in a dotfiles
repository without them. `~/.config/goat/.env`, the older home for both, still works
below the JSON files.

goat does **not** read the `.env` in the project it works on. That file belongs to the
project (its own keys, database URLs and so on), and a cloned repository could otherwise
point a provider's base URL at a server that would receive your key. Keys you kept in a
project's `.env` need to move to `auth.json`.

### 3. Run it

Start goat from the root of the project you want it to work on; that directory is its
workspace:

```sh
cd ~/code/my-project
goat
```

Type `/status` to check that the keys were found and where each came from:

```
Provider:  openai    unavailable (OPENAI_API_KEY not set)
Provider:  anthropic unavailable (ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN not set)
Provider:  deepseek  available (key from ~/.config/goat/auth.json)
```

`/model` picks a model, and `/help` lists the commands. See [Using goat](#using-goat) for
the rest.

### Optional: keep keys out of files

Instead of the key, give goat a command that prints it. goat runs it at startup with
`sh` (`cmd.exe` on Windows) and uses the one line it prints:

```json
{
  "openai": {"key_command": "op read op://Private/OpenAI/credential"},
  "anthropic": {"key_command": "security find-generic-password -w -s anthropic-api-key"},
  "deepseek": {"key_command": "pass show api/deepseek"}
}
```

That is 1Password, the macOS Keychain and `pass`. In the shell, the key's name plus
`_COMMAND` does the same, e.g.
`OPENAI_API_KEY_COMMAND="op read op://Private/OpenAI/credential"`.

The command runs on the terminal, so a password manager can ask for a fingerprint or a
passphrase. Input piped to goat, such as a prompt for `goat -p`, is not passed on to it,
and its error output only reaches a terminal, never a log goat's own error output is
redirected to.

### Optional: use a proxy or compatible API

`base_urls` in `config.json` sends a provider's requests, and its key, somewhere other
than its own API, such as a company proxy:

```json
{"base_urls": {"anthropic": "https://llm-proxy.example.com"}}
```

`OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL` and `DEEPSEEK_BASE_URL` in the shell do the same.
goat says at startup where the requests go; see [Safety](#safety) for the checks it makes.

### Optional: add other providers

Local servers and other services go under `providers` in `~/.config/goat/config.json`,
each under a name of your choosing:

```json
{
  "providers": {
    "ollama": {
      "api": "openai",
      "base_url": "http://localhost:11434/v1",
      "models": ["qwen3-coder"]
    },
    "my-router": {
      "api": "openai",
      "base_url": "https://router.example.com/v1",
      "models": ["moonshotai/kimi-k2"]
    },
    "work-proxy": {
      "api": "anthropic",
      "base_url": "https://llm.example.com",
      "key_env": "WORK_LLM_KEY"
    }
  }
}
```

with the keys in `auth.json` under the same names, e.g. `{"my-router": {"key": "sk-..."}}`
(or `/login my-router`). The [providers goat already knows](#other-providers), such as
OpenRouter, need no entry, only a key; an entry with the same name replaces goat's.

- `api` is the API the server speaks: `"openai"` for OpenAI-compatible Chat
  Completions (Ollama, LM Studio, vLLM, llama.cpp, LiteLLM, OpenRouter and most hosted
  services), or `"anthropic"` for the Anthropic Messages API.
- `base_url` is where requests go, checked like the `*_BASE_URL` variables.
- The key comes from `auth.json`, under the entry's name. Or `key_env` names a variable
  that holds it (a name in capitals, such as `WORK_LLM_KEY`), or `key_command` is a
  command that prints it. An entry with none of these sends no key, as a local server
  needs. `config.json` never holds a key itself, and an entry never gets a built-in
  provider's key unless `key_env` names it.
- `models` are offered by `/model`. Without it, `/model` offers whatever the server lists.
- Pick a model as `name:model`, e.g. `/model ollama:qwen3-coder` or
  `goat -m ollama:qwen3-coder`; a model only one entry lists needs no prefix.

goat sends OpenAI-compatible servers no effort setting, since many reject it.
Anthropic-compatible entries get Claude's thinking and effort settings only for models
named `claude-…`. Local servers often start with a small context window; if a long
conversation loses track, raise it in the server's settings. goat checks the file at
startup and stops with the line of a mistake, without printing the file. Entries show
up in `/status`. Entries can also go in `~/.config/goat/providers.json`, the older file
for them, as an object of the same shape; a name in both files is a mistake.

### Which value wins

- Settings come from the shell, then `config.json`, then the global `.env`. A setting
  the shell sets to a non-empty value keeps it.
- A provider's key is settled as a whole, in the same order: if the shell sets any of
  its key variables (or their `_COMMAND`), `auth.json` and `.env` are not used for it;
  otherwise if `auth.json` names it, `.env` is not. So if the shell supplies
  `ANTHROPIC_AUTH_TOKEN`, an `ANTHROPIC_API_KEY` elsewhere is not used.
- Within one place, a key that is set beats a command, which is then not run.
- `model` and `effort` in `config.json` are where goat starts when there is no saved
  conversation; `/model` and `/effort` choices still carry over. `GOAT_MODEL` and
  `GOAT_EFFORT` override the saved ones.
- A command that fails, prints nothing, prints more than one line or takes over 30
  seconds leaves its key unset, with a warning that never repeats its output; for
  Anthropic the other credential's command is tried next. On a timeout only the shell is
  stopped, so a command that runs several programs should `exec` the last one.

### Troubleshooting

- **`goat: command not found`**: the Go bin directory is not on your `PATH`; see
  [Install](#1-install).
- **`no API key found; ...`**: goat found no key in the shell, `auth.json` or the global
  `.env`, and it was not started at a terminal (with `-p`, or with piped input), where it
  would have offered `/login`. The message names the file it looked for; check the path. If it
  adds that goat does not read the `.env` in this folder, your keys are in the project's
  `.env`: move them to `auth.json`.
- **`.../config.json: line N: ...`** (or `auth.json`, `providers.json`): the line of a
  mistake, such as a missing comma or an unknown field. goat never prints the file's
  contents, so open it and look at that line.
- **`... is not a valid .env file: check line N`**: a line in the global `.env` is not
  `NAME=value`, or a quote is not closed.
- **A provider shows as unavailable in `/status`**: the reason follows it, such as a key
  that is not set or a base URL goat refuses.
- **Keys work in one folder but not another**: they are probably exported in one shell
  session or a project's `.env`; put them in `auth.json`.

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

### Shell commands

A line starting with `!` runs in the shell instead of going to the model: `!git status`,
`!npm test`. Only that line is the command; lines after it are sent as usual. It runs the way `run_bash` runs commands (with `bash -lc` in the workspace,
without the API keys in its environment and without a terminal to read from), but it is
not confirmed, since you typed it, and has no time limit: Ctrl-C stops it. Up to 40 lines of
its output are shown and `/output` shows the rest. The output also goes to the model with
your next message, so `!npm test` then `fix the failures` works; the footer says when
some is waiting. `/clear` and `/resume` drop it. A command started with `&` keeps running and is
listed by `/ps`.

Lines piped into goat run `!` commands only with `-y`, and a `-p` prompt is never one: it
goes to the model as it is.

### Typing while goat works

While a turn runs the prompt stays at the bottom of the screen, in the same frame as
between turns, under the spinner and above the footer; the reply streams in above it. It
takes a message, and Enter queues it. Queued text goes to the model with the next tool
results, so it can change course mid-turn; anything still queued when the turn ends is
sent as the next prompt. Slash commands and `!` commands wait until the turn is over. Up
on an empty line takes the last queued message back to edit. After Ctrl-C or a failed
turn nothing queued is sent; it is put back in the prompt instead, one message per line.
Queueing needs an interactive color terminal.

### Command line

```sh
goat                          # start a new conversation (/resume brings back the last one)
goat "fix the failing test"   # start and run this prompt first
goat -c                       # continue the last conversation instead
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
| `/login [provider]` (`/auth`) | Save a provider's API key to `auth.json` and start using it: pick the provider, then paste the key, which is not shown. For OpenAI, Anthropic, OpenRouter, xAI and Meta, choose **API key** first: signing in with your account is listed but not available yet. Needs a terminal. |
| `/status` | Model, effort, which providers are available, conversation and context size, and settings. |
| `/clear` (`/new`, `/reset`) | Start a new conversation under a fresh welcome banner; the old one is kept. |
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
| `/ps [stop n\|all]` (`/jobs`) | List the commands goat left running in the background, such as a dev server started with `&`, and stop them: pick one with the arrow keys and Enter, or `/ps stop 2` (by number, or by the group ID it lists), `/ps stop all`. The footer shows how many are running. |
| `/approve [on\|off]` | Show or change whether `run_bash` runs commands without asking. |
| `/theme [name]` | Switch to a built-in theme for this session, such as `one-dark`; with no name, reload the theme file and show which one is in use. |
| `/tools` | List the agent's tools. |
| `/history` | Show where the conversation history is written. |
| `/help` | List the commands and keys. |
| `/exit` (`/quit`) | Exit. |

The pickers (`/model`, `/effort`, `/output`, `/login`) take Up/Down to move, typing to filter,
Enter to choose and Esc to cancel.

The welcome banner stays in the scrollback as it was printed, so when `/model`, `/effort` or
`/resume` changes the model or effort, goat restates the banner's model row underneath. The
footer under the prompt always shows the current values.

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
  prefix, such as `openai:my-fine-tune` or `anthropic:claude-custom`. Providers added in
  [`providers.json`](#optional-add-other-providers) are picked the same way, as
  `name:model`. A model that one entry lists goes to that entry even when its name looks
  like a built-in one: if an Ollama entry lists `gpt-oss:20b`, or a proxy entry lists
  `claude-opus-5-5`, the bare name means the entry; `openai:` or `anthropic:` picks
  the built-in provider.
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

### Other providers

Beyond the three above, goat knows these OpenAI-compatible APIs. Each needs only a key,
from `/login`, `auth.json` under the name below, or the variable; `/status` lists the
ones that have one. They list no models of their own: `/model` asks the provider
(the picker's last entry, or `/model list`), and any model can be picked by name, e.g.
`/model openrouter:moonshotai/kimi-k2`.

| Name | Provider | Variable | Base URL |
| --- | --- | --- | --- |
| `openrouter` | OpenRouter | `OPENROUTER_API_KEY` | `https://openrouter.ai/api/v1` |
| `gemini` | Google Gemini | `GEMINI_API_KEY` | `https://generativelanguage.googleapis.com/v1beta/openai` |
| `xai` | xAI (Grok) | `XAI_API_KEY` | `https://api.x.ai/v1` |
| `meta` | Meta (Muse) | `META_API_KEY` | `https://api.meta.ai/v1` |
| `mistral` | Mistral | `MISTRAL_API_KEY` | `https://api.mistral.ai/v1` |
| `groq` | Groq | `GROQ_API_KEY` | `https://api.groq.com/openai/v1` |
| `together` | Together AI | `TOGETHER_API_KEY` | `https://api.together.ai/v1` |
| `fireworks` | Fireworks AI | `FIREWORKS_API_KEY` | `https://api.fireworks.ai/inference/v1` |
| `cerebras` | Cerebras | `CEREBRAS_API_KEY` | `https://api.cerebras.ai/v1` |
| `moonshot` | Moonshot (Kimi) | `MOONSHOT_API_KEY` | `https://api.moonshot.ai/v1` |
| `zai` | Z.ai (GLM) | `ZAI_API_KEY` | `https://api.z.ai/api/paas/v4` |
| `qwen` | Qwen (Alibaba Model Studio, international) | `DASHSCOPE_API_KEY` | `https://dashscope-intl.aliyuncs.com/compatible-mode/v1` |
| `minimax` | MiniMax | `MINIMAX_API_KEY` | `https://api.minimax.io/v1` |

Like the built-in keys, these variables are kept from `run_bash` commands once goat
knows them. A key for another region, such as Moonshot's or Qwen's mainland China API,
needs an entry of the same name in `config.json` with that region's `base_url`, e.g.
`"moonshot": {"api": "openai", "base_url": "https://api.moonshot.cn/v1", "key_env":
"MOONSHOT_API_KEY"}`. The entry replaces goat's, so it needs `key_env` when the key is in
the environment; a key saved with `/login` or in `auth.json` works without it. A model
an entry lists keeps its name: with an Ollama entry listing `mistral:7b`, that model is
Ollama's, not Mistral's.

## Conversations and state

goat keeps its state in `.goat/` inside the workspace:

| File | |
| --- | --- |
| `session.json` | The model and effort, restored on the next start, and the current conversation, which `goat -c` continues |
| `session.prev.json` | The previous conversation, for `/resume` |
| `history.jsonl` | An append-only log of every prompt, reply and tool call |
| `prompts.jsonl` | The prompts typed at the interactive prompt, for Up |
| `operations.json`, `snapshots/` | The record of file changes and the snapshots `/undo` restores from |

Each start begins a new conversation and keeps the last one in `session.prev.json`, so an
old conversation is not sent again, and paid for again, with every request; `/resume` or
`goat -c` brings it back.

When the conversation passes `GOAT_AUTO_COMPACT_TOKENS`, goat compacts it automatically,
the same way `/compact` does: it is summarized into a fresh, smaller context. goat checks
before each prompt and after each round of tool calls, so a long task is compacted
partway through and carries on from the summary. If `session.json` or `operations.json` cannot be read, goat moves it
aside with a `.corrupt-<time>` suffix, warns, and starts fresh.

## Safety

- File tools only accept paths inside the workspace, refuse symlinks that lead outside it,
  and cannot touch `.goat/`. They also refuse `.env`, `.env.*` and `.envrc` (templates such
  as `.env.example` stay readable), and `grep_files` skips them, so credentials never reach
  the model through a file tool.
- A provider's key goes to its own API unless its base URL says otherwise, and only your
  shell or goat's own config directory (`base_urls` in `config.json`, or `.env`) can say
  so. Likewise only `config.json` (or `providers.json`) there adds providers, never a
  file in the workspace, and an entry's key goes only to its own `base_url`. goat refuses a base URL that is not
  http(s) or that carries a user name or password. For any other host it says at startup,
  and in `/status`, where the requests go and which file set the URL, flagging plain http
  to another machine as unencrypted (http to `localhost` is fine); an entry is
  mentioned at startup only when its key would go unencrypted. It shows only the
  scheme and host, since some proxies take a token in the path, and shortens the URL the
  same way in error messages and the history. Requests that carry a key never follow a
  redirect, which could otherwise take the key to a host goat never checked, and never
  send plain http through `HTTP_PROXY`, where the proxy would read the key (https goes
  through it as a tunnel the proxy cannot read).
- `/login` never shows the key as it is typed or pasted, only how many characters it
  has; it writes `auth.json` in one step, readable only by you, keeping what else the
  file holds; and an error from checking the key has the key cut out of it.
- Keys from `auth.json` never enter goat's environment, so no command it runs can
  inherit them. `run_bash` commands also do not inherit the shell's API keys (including
  the variables entries name in `key_env`), anything loaded from `~/.config/goat/.env`,
  the providers' `*_BASE_URL` (a proxy may take a token in it), or other variables named
  like credentials (`*_API_KEY`, `*_SECRET`, `*_AUTH_TOKEN`, `*_PASSWORD`, a key
  command's `*_COMMAND` and similar). File tools never open or search goat's config
  directory, even when the workspace contains it (goat started in your home directory),
  but a command can read `auth.json` like any other file you can read: approving
  commands is what guards it. goat always asks before a command
  that names its config directory (`config/goat`, `XDG_CONFIG_HOME`, its full path, or a
  relative path from a directory above it) or runs inside it, even when commands
  otherwise run without asking. The check matches text, so quoting, globs, variables or
  a `cd` earlier in the command get past it.
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
- What a command leaves running in the background, such as a server started with `&`
  (with its output redirected to a file, or it stops at its next write), keeps running
  after the command returns. goat keeps track of it by process group: `/ps` lists and
  stops it, and goat stops it, saying so, when it exits, when its terminal closes or when
  it gets SIGTERM (a command still running then is stopped too; under `nohup` a hangup
  stays ignored). A process that starts a session of its own, such as a
  daemon, leaves the group and is not tracked. On Windows nothing is tracked.

## Themes

goat's look is black and white by default; the one exception is syntax highlighting in
code blocks, which uses color. For color everywhere, goat has a built-in **One Dark**
theme, after Atom's: blue accents and borders, red headings, green and red for success,
errors and diffs, yellow for warnings and approvals.

```sh
/theme one-dark        # switch for this session; /theme default switches back
"theme": "one-dark"    # in ~/.config/goat/config.json, to start with it
```

Case, spaces, hyphens and underscores in a built-in theme's name do not matter, so
`onedark` and `One Dark` are one-dark too. A misspelled name is not applied; `/theme` and
the `"theme"` setting say which theme it probably meant.

All styling goes through a small style engine
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

goat reads the file `GOAT_THEME` names (unless it names a built-in theme), or else
`~/.config/goat/theme.json` (`$XDG_CONFIG_HOME/goat/theme.json` when that is set). It is
layered over the default theme,
[`internal/style/themes/default.json`](internal/style/themes/default.json), property by
property, the way CSS cascades: roles you leave out keep their defaults, and
`font-weight: normal` turns a default bold off. To adjust a built-in theme instead, name
it in `"extends"`, e.g. `{"extends": "one-dark", "styles": {"heading": "color: #61afef"}}`;
the built-in themes are in [`internal/style/themes`](internal/style/themes). `/theme`
reloads the file and lists the built-in themes.

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

goat's settings go in `~/.config/goat/config.json` (`$XDG_CONFIG_HOME/goat/config.json`
when that is set), which never holds keys:

```json
{
  "theme": "one-dark",
  "model": "anthropic:claude-opus-5-5",
  "effort": "high",
  "auto_approve_bash": false,
  "code_mode": false,
  "auto_compact_tokens": 120000,
  "base_urls": {"anthropic": "https://llm-proxy.example.com"},
  "providers": {"ollama": {"api": "openai", "base_url": "http://localhost:11434/v1"}}
}
```

| Field | Default | Purpose | Variable |
| --- | --- | --- | --- |
| `theme` | `default` | A built-in theme (`default`, `one-dark`) or the full path of a theme file (`~/` works) | `GOAT_THEME` |
| `model` | the first provider with a key | Model to start with when there is no saved conversation | `GOAT_MODEL` overrides the saved one |
| `effort` | `medium` | Effort to start with when there is no saved conversation | `GOAT_EFFORT` overrides the saved one |
| `auto_approve_bash` | `false` | Run `run_bash` commands without asking | `GOAT_AUTO_APPROVE_BASH` |
| `code_mode` | `false` | Start in code mode | `GOAT_CODEMODE` |
| `auto_compact_tokens` | `40000` (OpenAI), `60000` (Claude, DeepSeek) | Conversation size that triggers automatic compaction; lower it for small tokens-per-minute limits | `GOAT_AUTO_COMPACT_TOKENS` |
| `anthropic_fallbacks` | `true` | Server-side refusal fallbacks for Claude models | `GOAT_ANTHROPIC_FALLBACKS` (`on`/`off`) |
| `anthropic_cache_ttl` | `"5m"` | Lifetime of Claude prompt cache entries, `"5m"` or `"1h"` | `GOAT_ANTHROPIC_CACHE_TTL` |
| `reduced_motion` | `false` | Keep the spinner still (it updates once a second) | `GOAT_REDUCED_MOTION` |
| `base_urls` | | `openai`, `anthropic` or `deepseek` at a proxy or a compatible API (see [Safety](#safety) for the checks) | `OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL`, `DEEPSEEK_BASE_URL` |
| `providers` | | Other providers (see [Setup](#optional-add-other-providers)) | |

Each variable, set in the shell, wins over its field. goat refuses a field it does not
know, and a field named like a key points to `auth.json`. A few variables have no field:
`OPENAI_MODEL` and `OPENAI_REASONING_EFFORT` (older startup defaults), `NO_COLOR` (turn
styling off) and `GOAT_FORCE_COLOR` (style output even when stdout is not a terminal,
for example when piping through a pager that understands colors).

The keys go in `~/.config/goat/auth.json`, by provider, as a `key`, an Anthropic
`auth_token`, or a `key_command` (see [Setup](#2-add-an-api-key)). The shell's
`OPENAI_API_KEY`, `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` and `DEEPSEEK_API_KEY`,
and their `_COMMAND` forms, win over it.

`~/.config/goat/.env`, the older way to set both, is still read below the JSON files:
any variable above as `NAME=value`. `HOME` and `XDG_CONFIG_HOME` come only from the
environment, since they locate the files.

goat also reads these files, all optional (`~/.config` is `$XDG_CONFIG_HOME` when set):

| File | |
| --- | --- |
| `~/.config/goat/config.json` | Settings and providers; never keys |
| `~/.config/goat/auth.json` | API keys or commands that print them; readable only by you |
| `~/.config/goat/.env` | Older home for keys and settings, below the JSON files. The workspace's `.env` is never read |
| `~/.config/goat/providers.json` | Older home for providers |
| `AGENTS.md` (or `GOAT.md`, `CLAUDE.md`) in the workspace | Project instructions |
| `~/.config/goat/AGENTS.md` | Your own instructions for every project |
| `~/.config/goat/theme.json` | Your own theme |
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
