package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/terminal"
	"github.com/azs06/goat/internal/workspace"

	"github.com/azs06/goat/internal/style"
)

type config struct {
	model             provider.Model
	reasoningEffort   provider.Effort
	autoCompactTokens int64
	conversation      conversationState
	// ephemeral is set for a -p run that does not continue the saved conversation; its
	// session is never written.
	ephemeral bool
	// staleConversation is set when the saved conversation was built with a different
	// system prompt or tool set; it is compacted before it is used again.
	staleConversation bool
	// promptFingerprint is the fingerprint the conversation was built with.
	promptFingerprint string

	// providers holds the API clients, created on first use.
	providers map[string]provider.Provider

	input       *lineReader
	interactive bool
	approver    *bashApprover

	lastPrompt string
	// promptHistory holds the prompts Up brings back, oldest first.
	promptHistory []string
	// lastModelChoices are the models the last /model listing numbered.
	lastModelChoices []provider.Model
	// planMode, toggled with /plan or started with --plan, lets the agent read and
	// investigate but not change anything: tools that would change files, and run_bash,
	// are refused, and each prompt tells the model so. The tool list itself stays the
	// same, so switching costs no prompt cache.
	planMode bool
}

// provider returns the client for a provider, creating it the first time. Providers
// without credentials are refused.
func (c *config) provider(name string) (provider.Provider, error) {
	if p, ok := c.providers[name]; ok {
		return p, nil
	}
	if !provider.Available(name) {
		return nil, provider.MissingCredentialsError(name)
	}

	var p provider.Provider
	switch name {
	case provider.OpenAI:
		p = provider.NewOpenAI()
	case provider.Anthropic:
		p = provider.NewAnthropic()
	case provider.DeepSeek:
		p = provider.NewDeepSeek()
	default:
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	if c.providers == nil {
		c.providers = map[string]provider.Provider{}
	}
	c.providers[name] = p
	return p, nil
}

func (c *config) currentProvider() (provider.Provider, error) {
	if c.model.IsZero() {
		return nil, fmt.Errorf("no model is selected; use /model")
	}
	return c.provider(c.model.Provider)
}

// startupModel picks the first usable model in order: the --model flag, GOAT_MODEL, the
// saved session, OPENAI_MODEL, then a default for whichever provider has credentials.
// Candidates whose provider has no API key are skipped.
func startupModel(saved *savedSession, flag string) (provider.Model, string) {
	type candidate struct{ source, raw string }
	var candidates []candidate
	if raw := strings.TrimSpace(flag); raw != "" {
		candidates = append(candidates, candidate{"--model", raw})
	}
	if raw := strings.TrimSpace(os.Getenv("GOAT_MODEL")); raw != "" {
		candidates = append(candidates, candidate{"GOAT_MODEL", raw})
	}
	if saved != nil && strings.TrimSpace(saved.Model) != "" {
		candidates = append(candidates, candidate{"saved session", saved.Model})
	}
	if raw := strings.TrimSpace(os.Getenv("OPENAI_MODEL")); raw != "" {
		candidates = append(candidates, candidate{"OPENAI_MODEL", "openai:" + strings.TrimPrefix(raw, "openai:")})
	}

	for _, c := range candidates {
		ref, err := provider.ResolveModel(c.raw)
		if err != nil {
			warnf("ignoring the model from %s: %v", c.source, err)
			continue
		}
		if !provider.Available(ref.Provider) {
			switch c.source {
			case "GOAT_MODEL":
				warnf("ignoring GOAT_MODEL=%s: %v", c.raw, provider.MissingCredentialsError(ref.Provider))
			case "--model":
				warnf("ignoring --model %s: %v", c.raw, provider.MissingCredentialsError(ref.Provider))
			}
			continue
		}
		return ref, c.source
	}
	return provider.DefaultModel(), "default"
}

// startupEffort picks the effort in order: the --effort flag, GOAT_EFFORT, the saved
// session, OPENAI_REASONING_EFFORT, then the default.
func startupEffort(saved *savedSession, flag string) provider.Effort {
	candidates := []struct{ source, value string }{{"--effort", flag}, {"GOAT_EFFORT", os.Getenv("GOAT_EFFORT")}}
	if saved != nil {
		candidates = append(candidates, struct{ source, value string }{"saved session", saved.Effort})
	}
	candidates = append(candidates, struct{ source, value string }{"OPENAI_REASONING_EFFORT", os.Getenv("OPENAI_REASONING_EFFORT")})

	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.value) == "" {
			continue
		}
		effort, err := provider.ParseEffort(candidate.value)
		if err != nil {
			warnf("invalid effort from %s: %v", candidate.source, err)
			continue
		}
		return effort
	}
	return provider.DefaultEffort
}

// loadConfig builds the startup configuration from the saved session, the environment
// and the --model and --effort flags in opts.
func loadConfig(opts cliOptions) *config {
	saved, err := loadSessionFile(sessionPath)
	if err != nil {
		warnf("failed to load the saved session: %v", err)
	}

	model, _ := startupModel(saved, opts.model)
	autoCompactTokens, err := parseAutoCompactTokens(os.Getenv("GOAT_AUTO_COMPACT_TOKENS"))
	if err != nil {
		warnf("invalid GOAT_AUTO_COMPACT_TOKENS: %v; using the default", err)
		autoCompactTokens = 0
	}

	cfg := &config{model: model, reasoningEffort: startupEffort(saved, opts.effort), autoCompactTokens: autoCompactTokens, planMode: opts.plan}
	restoreSavedConversation(cfg, saved)
	return cfg
}

// restoreSavedConversation continues the saved conversation when it belongs to the
// active model's provider. Otherwise the previous conversation is tried, and if neither
// fits a new conversation starts with the saved one kept for /resume. Without a session
// file it falls back to the response ID in the history log, which is how OpenAI
// conversations were resumed before.
func restoreSavedConversation(cfg *config, saved *savedSession) {
	if saved == nil {
		if cfg.model.Provider == provider.OpenAI {
			if responseID, conversationID, err := latestResumableConversation(); err != nil {
				warnf("failed to load saved conversation context: %v", err)
			} else if responseID != "" {
				// Continue the last OpenAI conversation in the history log, under the
				// history scope it was recorded in (none, from before sessions existed).
				activeConversationID = conversationID
				cfg.conversation.conversation = provider.Conversation{Provider: provider.OpenAI, ResponseID: responseID}
				return
			}
		}
		activeConversationID = newConversationID()
		return
	}

	if saved.Conversation.IsEmpty() || saved.Conversation.Provider == cfg.model.Provider {
		adoptSavedConversation(cfg, saved)
		return
	}

	// The saved conversation belongs to another provider. If the previous one matches,
	// swap the two; otherwise start fresh and keep the saved one for /resume.
	previous, _ := loadSessionFile(previousSessionPath)
	if err := sessionSaver(previousSessionPath, *saved); err != nil {
		warnf("failed to keep the saved conversation: %v", err)
		return
	}
	if previous != nil && !previous.Conversation.IsEmpty() && previous.Conversation.Provider == cfg.model.Provider {
		adoptSavedConversation(cfg, previous)
		saveSession(cfg)
		fmt.Println(formatNotice(fmt.Sprintf("Continuing the earlier %s conversation; the %s one is kept and /resume switches to it.", cfg.model.Provider, saved.Conversation.Provider)))
		return
	}

	startConversation(cfg, conversationState{})
	if provider.Available(saved.Conversation.Provider) {
		fmt.Println(formatNotice(fmt.Sprintf("The saved %s conversation does not carry over to %s; it was kept, and /resume restores it.", saved.Model, cfg.model)))
	} else {
		fmt.Println(formatNotice(fmt.Sprintf("The saved %s conversation was kept; /resume restores it once %s.", saved.Model, provider.MissingCredentialsError(saved.Conversation.Provider))))
	}
}

func parseAutoCompactTokens(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}

	tokens, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || tokens <= 0 {
		return 0, fmt.Errorf("expected a positive token count, got %q", raw)
	}
	return tokens, nil
}

func envFlag(name string) bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return err == nil && enabled
}

// activeInputGuard is the guard of the command or turn currently running, if any.
var activeInputGuard *terminal.InputGuard

func runWithInputGuard(cbreak bool, work func() error) error {
	guard, err := terminal.BeginInputGuard(cbreak)
	if err != nil {
		return err
	}
	activeInputGuard = guard
	defer func() {
		activeInputGuard = nil
		if closeErr := guard.Close(); closeErr != nil {
			warnf("failed to restore terminal input state: %v", closeErr)
		}
	}()

	return work()
}

// suspendInputGuard turns terminal echo back on so the user can answer a question in
// the middle of a turn, and returns the function that turns it off again.
func suspendInputGuard() func() {
	resumeSpinner := pauseSpinner()
	capture := activeKeyCapture
	capture.pause()
	suspended := time.Now()
	guard := activeInputGuard
	if err := guard.Suspend(); err != nil {
		warnf("failed to restore terminal echo: %v", err)
	}
	return func() {
		inputWaitTotal += time.Since(suspended)
		if err := guard.Resume(); err != nil {
			warnf("failed to disable terminal echo: %v", err)
		}
		capture.resume()
		resumeSpinner()
	}
}

// inputWaitTotal is the total time spent waiting on the user's answers mid-turn, so
// a tool's elapsed time can leave out how long its approval took.
var inputWaitTotal time.Duration

// ask shows a question and reads one line of answer while a command runs.
func (c *config) ask(ctx context.Context, question string) (string, error) {
	if c.input == nil {
		return "", io.EOF
	}
	resume := suspendInputGuard()
	defer resume()

	fmt.Print(question)
	answer, err := c.input.ReadLine(ctx)
	if err != nil {
		fmt.Println()
	}
	return strings.TrimSpace(answer), err
}

func runPrompt(ctx context.Context, cfg *config, prompt string) error {
	cfg.lastPrompt = prompt

	p, err := cfg.currentProvider()
	if err != nil {
		return err
	}

	if _, err := maybeAutoCompactConversation(ctx, cfg); err != nil {
		if ctx.Err() != nil {
			return err
		}
		warnf("%v; continuing with the full conversation", err)
	}

	opts := turnOptions{
		model:    cfg.model.ID,
		effort:   cfg.reasoningEffort,
		planMode: cfg.planMode,
		// Save after every tool round and whatever the outcome, so a crash or Ctrl-C
		// never loses work the model already did.
		checkpoint: func(state conversationState) {
			cfg.conversation = state
			saveSession(cfg)
		},
	}
	defer saveSession(cfg)
	if cfg.approver != nil {
		opts.approveBash = cfg.approver.approve
	}
	// Messages queued while the turn works go to the model with the next tool results.
	if cfg.interactive && style.Enabled() {
		opts.takeSteering = takeQueuedText
	}

	next, err := sendPromptStream(ctx, p, prompt, cfg.conversation, opts)
	cfg.conversation = next
	if err == nil || !provider.IsThinkingBindingError(err) {
		return err
	}

	// The API no longer accepts this conversation's thinking blocks. Carry it over in a
	// summary and try once more.
	fmt.Println(formatNotice("The conversation could not be continued as-is; compacting it and retrying."))
	summaryConversation, summary, compactErr := compactConversation(ctx, p, cfg.model.ID, "")
	if compactErr != nil {
		return fmt.Errorf("%w (compacting it failed too: %v; use /clear)", err, compactErr)
	}
	applyCompactedConversation(cfg, summaryConversation, summary, "recovery")
	next, err = sendPromptStream(ctx, p, prompt, cfg.conversation, opts)
	cfg.conversation = next
	return err
}

// handleInput runs one command or prompt. Ctrl-C cancels it instead of killing the goat.
func handleInput(cfg *config, text string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cmd, args, err := lookupCommand(text)
	if err != nil {
		return err
	}

	// On an interactive color terminal, what is typed while the work runs is queued.
	queueing := cfg.interactive && style.Enabled()
	started := time.Now()
	before, _ := loadConversationHistoryEntries()
	err = runWithInputGuard(queueing, func() error {
		if queueing {
			fmt.Print(bracketedPasteOn)
			defer fmt.Print(bracketedPasteOff)
			startKeyCapture(ctx, cfg.input)
			defer stopKeyCapture()
		}
		if cmd != nil {
			return cmd.callback(ctx, cfg, args)
		}
		return runPrompt(ctx, cfg, text)
	})
	if cmd == nil && err == nil && style.Enabled() {
		if elapsed := time.Since(started); elapsed >= time.Second {
			stopSpinner()
			fmt.Fprintln(activityOut, "\n"+renderWorked(elapsed, turnCostText(before)))
		}
	}
	return err
}

// printWelcome shows the banner and the notices about the restored state.
func printWelcome(cfg *config) {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	git := ""
	if state, ok := readGitState(); ok {
		git = state.footerText()
	}
	fmt.Println(renderBanner(cfg.model.String(), string(cfg.reasoningEffort), shortenHome(cwd, home), git, terminalWidth()))
	if !cfg.conversation.conversation.IsEmpty() {
		fmt.Println(formatNotice("Resumed saved conversation context."))
	}
	if sources := projectInstructionSources(); len(sources) > 0 {
		fmt.Println(formatNotice("Project instructions: " + strings.Join(sources, ", ") + " (sent with each new conversation)."))
	}
	if cfg.approver.autoApprove {
		message := "run_bash commands are auto-approved (GOAT_AUTO_APPROVE_BASH)."
		if style.Enabled() {
			message = formatWarning(message)
		}
		fmt.Println(message)
	}
}

// footerParts is the status shown under the interactive prompt.
func footerParts(cfg *config) []string {
	parts := []string{cfg.model.String(), "effort " + string(cfg.reasoningEffort)}
	if cfg.planMode {
		parts = append(parts, "plan mode")
	}
	if state, ok := readGitState(); ok {
		parts = append(parts, state.footerText())
	}
	if !cfg.conversation.conversation.IsEmpty() {
		if entries, err := loadConversationHistoryEntries(); err == nil {
			parts = append(parts, "~"+formatTokenCount(estimateConversationTokens(entries))+" ctx")
		}
	}
	if cfg.approver != nil && cfg.approver.autoApprove {
		parts = append(parts, "auto-approve on")
	}
	return append(parts, "? for shortcuts")
}

// readPrompt shows the prompt and reads one line. On an interactive color terminal the
// prompt sits between two rules with a status footer below, and once submitted the
// frame is replaced by the highlighted message. There the line is typed in the prompt
// editor, which completes slash commands. gap puts a blank line above the prompt.
// On an interactive terminal Ctrl-C returns context.Canceled instead of killing goat.
func readPrompt(cfg *config, gap bool) (string, error) {
	ctx := context.Background()
	if cfg.interactive {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
	}

	fancy := cfg.interactive && style.Enabled()
	if !fancy {
		fmt.Print("G.O.A.T > ")
		text, err := cfg.input.ReadLine(ctx)
		if err != nil {
			fmt.Println()
		}
		return text, err
	}

	width := terminalWidth()
	if gap {
		fmt.Println()
	}
	footer := renderFooter(footerParts(cfg), width)
	if text, ok, err := editPrompt(ctx, cfg.input, cfg.promptHistory, footer, width); ok {
		if err == nil && strings.TrimSpace(text) != "" {
			cfg.promptHistory = rememberPrompt(cfg.promptHistory, text)
			fmt.Println(renderUserMessage(text, width))
		}
		return text, err
	}

	// The terminal could not be put in cbreak mode: read a whole line with the
	// terminal echoing it.
	fmt.Print(renderPromptFrame(footer, width))
	text, err := cfg.input.ReadLine(ctx)
	if err != nil {
		// The cursor is still on the prompt line: clear from the rule above it down.
		fmt.Print("\r\x1b[1A\x1b[J")
		return text, err
	}
	if !cfg.input.lastTerminated {
		// Ended with Ctrl-D rather than Enter: nothing moved the cursor below the input
		// yet, which the cleanup expects.
		fmt.Print("\r\n")
	}
	fmt.Print(renderPromptCleanup(text, width))
	if strings.TrimSpace(text) != "" {
		fmt.Println(renderUserMessage(text, width))
	}
	return text, nil
}

// exitConfirmWindow is how soon a second Ctrl-C at the prompt has to follow the first
// to exit.
const exitConfirmWindow = 2 * time.Second

func main() {
	opts, err := parseCLI(os.Args[1:], os.Stdout)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "goat: "+err.Error())
		os.Exit(2)
	}
	if opts.showVersion {
		fmt.Println("goat " + goatVersion())
		return
	}

	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Fatalf("Error loading .env file: %v", err)
	}
	if values, err := godotenv.Read(); err == nil {
		workspace.HideFromCommands(slices.Collect(maps.Keys(values))...)
	}
	if len(provider.AvailableNames()) == 0 {
		log.Fatal("no API key found; set OPENAI_API_KEY, ANTHROPIC_API_KEY (or ANTHROPIC_AUTH_TOKEN) or DEEPSEEK_API_KEY, in the environment or a .env file")
	}

	provider.Warnf = warnf
	workspace.Warnf = warnf
	terminal.Warnf = warnf
	applyTheme()
	cfg := loadConfig(opts)
	cfg.input = newLineReader(os.Stdin)
	cfg.promptHistory = loadPromptHistory()
	cfg.interactive = terminal.StdinIsTerminal()
	cfg.approver = &bashApprover{input: cfg.input, interactive: cfg.interactive, autoApprove: opts.autoApprove || envFlag("GOAT_AUTO_APPROVE_BASH")}
	argumentCompletions = commandArgumentCompletions(cfg)
	projectInstructions = projectInstructionsText

	if opts.print {
		// A one-shot run never asks anything: stdin may hold the prompt itself.
		cfg.interactive = false
		cfg.approver.interactive = false
		os.Exit(runOneShot(cfg, opts))
	}
	if opts.newConversation && !cfg.conversation.conversation.IsEmpty() {
		replaceConversation(cfg, conversationState{})
		recordHistory(historyEntry{Type: "reset", Provider: cfg.model.Provider})
	}
	if opts.prompt != "" {
		// A prompt on the command line runs first, as if it had been queued.
		turnQueue.mu.Lock()
		turnQueue.items = append(turnQueue.items, opts.prompt)
		turnQueue.mu.Unlock()
	}

	printWelcome(cfg)

	gap := true
	var lastInterrupt time.Time
	for {
		// Messages queued during the last turn run before the prompt comes back.
		var err error
		text, queued := nextQueuedInput()
		if queued {
			fmt.Println("\n" + renderUserMessage(text, terminalWidth()))
		} else {
			text, err = readPrompt(cfg, gap)
		}
		gap = true
		if errors.Is(err, context.Canceled) {
			if time.Since(lastInterrupt) < exitConfirmWindow {
				fmt.Println("Closing the Goat... Goodbye!")
				return
			}
			lastInterrupt = time.Now()
			fmt.Println(formatNotice("Press Ctrl-C again to exit."))
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "Error reading input: %v\n", err)
			}
			fmt.Println("Closing the Goat... Goodbye!")
			return
		}
		if strings.TrimSpace(text) == "" {
			gap = false
			continue
		}

		err = handleInput(cfg, text)
		stopSpinner()
		if err != nil {
			// Nothing queued is sent after a turn that was interrupted or failed; it
			// waits in the prompt instead.
			returnQueueToPrompt(cfg.input)
		}
		switch {
		case err == nil:
		case errors.Is(err, errExitRequested):
			fmt.Println("Closing the Goat... Goodbye!")
			return
		case errors.Is(err, context.Canceled):
			if style.Enabled() {
				fmt.Println("\n" + formatWarning("Interrupted "+style.Glyphs().Dot+" tell goat what to do instead"))
			} else {
				fmt.Println("\nInterrupted.")
			}
		default:
			fmt.Println(formatError(err.Error()))
		}
	}
}
