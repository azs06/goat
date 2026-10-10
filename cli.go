package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/terminal"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3"; otherwise it is
// taken from the module or VCS information Go records in the binary.
var version = ""

func goatVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	// A pseudo-version (v0.0.0-<time>-<commit>) says no more than the commit below.
	if v := info.Main.Version; v != "" && v != "(devel)" && !strings.HasPrefix(v, "v0.0.0-") {
		return v
	}
	revision, modified := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		revision += "-dirty"
	}
	return "dev-" + revision
}

// cliOptions are the command-line arguments.
type cliOptions struct {
	help, showVersion bool
	model, effort     string
	// print runs prompt once without the interactive prompt and exits.
	print bool
	// continueSession makes a print run continue the saved conversation instead of
	// starting its own.
	continueSession bool
	newConversation bool
	autoApprove     bool
	plan            bool
	codeMode        bool
	prompt          string
}

const usageText = `goat: a coding agent for your terminal.

Usage:
  goat [flags] [prompt]       start goat; a prompt given here runs first
  goat -p [flags] [prompt]    run one prompt, print the reply and exit
                              (the prompt is read from stdin when not given)

Flags:
  -m, --model MODEL    model to use, e.g. claude-opus-5-5 or openai:gpt-6-luna
  -e, --effort LEVEL   reasoning effort: none, minimal, low, medium, high, xhigh, max
  -p, --print          run one prompt non-interactively and exit
  -c, --continue       with -p, continue the saved conversation instead of a new one
  -n, --new            start a new conversation (the previous one stays for /resume)
  -y, --yes            run run_bash commands without asking (GOAT_AUTO_APPROVE_BASH=1)
      --plan           start in plan mode: read and plan, change nothing
      --codemode       start in code mode: batch tool calls in run_code scripts
  -v, --version        print the version and exit
  -h, --help           show this help

API keys come from OPENAI_API_KEY, ANTHROPIC_API_KEY (or ANTHROPIC_AUTH_TOKEN) and
DEEPSEEK_API_KEY, in the environment or a .env file. See the readme for the rest.
`

// parseCLI reads the command line. Flags may come before or after the prompt words.
func parseCLI(args []string, output io.Writer) (cliOptions, error) {
	var opts cliOptions
	fs := flag.NewFlagSet("goat", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, name := range []string{"h", "help"} {
		fs.BoolVar(&opts.help, name, false, "")
	}
	for _, name := range []string{"v", "version"} {
		fs.BoolVar(&opts.showVersion, name, false, "")
	}
	for _, name := range []string{"m", "model"} {
		fs.StringVar(&opts.model, name, "", "")
	}
	for _, name := range []string{"e", "effort"} {
		fs.StringVar(&opts.effort, name, "", "")
	}
	for _, name := range []string{"p", "print"} {
		fs.BoolVar(&opts.print, name, false, "")
	}
	for _, name := range []string{"c", "continue"} {
		fs.BoolVar(&opts.continueSession, name, false, "")
	}
	for _, name := range []string{"n", "new"} {
		fs.BoolVar(&opts.newConversation, name, false, "")
	}
	for _, name := range []string{"y", "yes"} {
		fs.BoolVar(&opts.autoApprove, name, false, "")
	}
	fs.BoolVar(&opts.plan, "plan", false, "")
	fs.BoolVar(&opts.codeMode, "codemode", false, "")

	var words []string
	for {
		if err := fs.Parse(args); err != nil {
			return opts, fmt.Errorf("%v (see goat --help)", err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		if rest[0] == "--" {
			words = append(words, rest[1:]...)
			break
		}
		words = append(words, rest[0])
		args = rest[1:]
	}
	opts.prompt = strings.TrimSpace(strings.Join(words, " "))

	if opts.help {
		fmt.Fprint(output, usageText)
		return opts, flag.ErrHelp
	}
	if opts.effort != "" {
		if _, err := provider.ParseEffort(opts.effort); err != nil {
			return opts, err
		}
	}
	if opts.model != "" {
		if _, err := provider.ResolveModel(opts.model); err != nil {
			return opts, err
		}
	}
	if opts.continueSession && opts.newConversation {
		return opts, errors.New("--continue and --new cannot be used together")
	}
	return opts, nil
}

// readPromptFromStdin reads a -p prompt that was piped in.
func readPromptFromStdin() (string, error) {
	if terminal.StdinIsTerminal() {
		return "", errors.New("-p needs a prompt: pass it as an argument or pipe it in")
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read the prompt from stdin: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// runOneShot runs a single prompt for -p and returns the exit code. Unless --continue
// is given it works in a conversation of its own and leaves the saved session alone,
// so a scripted run never replaces what the interactive goat resumes.
func runOneShot(cfg *config, opts cliOptions) int {
	prompt := opts.prompt
	if prompt == "" {
		var err error
		if prompt, err = readPromptFromStdin(); err != nil {
			fmt.Fprintln(os.Stderr, formatError(err.Error()))
			return 2
		}
	}
	if prompt == "" {
		fmt.Fprintln(os.Stderr, formatError("the prompt is empty"))
		return 2
	}

	if !opts.continueSession {
		cfg.ephemeral = true
		activeConversationID = newConversationID()
		cfg.conversation = conversationState{}
		cfg.staleConversation = false
		cfg.promptFingerprint = promptFingerprint()
	}

	activityOut = os.Stderr
	err := handleInput(cfg, prompt)
	stopSpinner()
	switch {
	case err == nil, errors.Is(err, errExitRequested):
		return 0
	default:
		fmt.Fprintln(os.Stderr, formatError(err.Error()))
		return 1
	}
}
