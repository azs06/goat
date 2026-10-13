package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	// ephemeral is set for a -p run that does not continue the saved conversation, and
	// for a start that could not keep the saved one for /resume; its session is never
	// written.
	ephemeral bool
	// staleConversation is set when the saved conversation was built with a different
	// system prompt or tool set; it is compacted before it is used again.
	staleConversation bool
	// promptFingerprint is the fingerprint the conversation was built with.
	promptFingerprint string

	// providers holds the API clients, created on first use. providersMu guards it,
	// since turns running beside the main one look clients up too.
	providersMu sync.Mutex
	providers   map[string]provider.Provider

	input       *lineReader
	interactive bool
	approver    *bashApprover

	lastPrompt string
	// commandText is the line of the command being run, for commands that need its
	// spacing and quotes kept, such as /verify.
	commandText string
	// promptHistory holds the prompts Up brings back, oldest first.
	promptHistory []string
	// lastModelChoices are the models the last /model listing numbered.
	lastModelChoices []provider.Model
	// planMode, toggled with /plan or started with --plan, lets the agent read and
	// investigate but not change anything: tools that would change files, and run_bash,
	// are refused, and each prompt tells the model so. The tool list itself stays the
	// same, so switching costs no prompt cache.
	planMode bool
	// codeMode, toggled with /codemode or started with --codemode or GOAT_CODEMODE,
	// lets the agent run run_code scripts that call the tools from JavaScript. Like
	// plan mode it is told to the model in each prompt; the tool list stays the same.
	codeMode bool
	// pendingSaved is the saved session goat started without a model to continue it
	// on. It is left on disk untouched, and restored once /login or /model picks a model.
	pendingSaved *savedSession
	// continueSaved, set by -c, makes goat continue the saved conversation. Otherwise
	// each start begins a new one and keeps the saved one for /resume, so an old
	// conversation is not sent again, and paid for again, with every request.
	continueSaved bool
	// keptPrevious is set when this start set the saved conversation aside for /resume.
	keptPrevious bool
	// oneShot is set for a -p run, whose prompt is never a "!" command.
	oneShot bool
	// pendingShell holds the output of the "!" commands run since the last message,
	// ready for the model; it goes with the next message.
	pendingShell []string
	// promptImages are the images pasted at the prompt for the next message, and
	// imageCount numbers their labels for the session. lastImages went with lastPrompt,
	// for /retry.
	promptImages []promptImage
	imageCount   int
	lastImages   []provider.Image
	// agents runs the session's subagents; nil until main starts it.
	agents *agentHost
	// agentNotices are the reports of background subagents that finished between
	// turns, ready for the model; they go with the next message.
	agentNotices []string
	// verifyPending is set when a turn's verify was put off because a general subagent
	// was still changing the workspace; the next turn runs it.
	verifyPending bool
}

// provider returns the client for a provider, creating it the first time. Providers
// without credentials are refused.
func (c *config) provider(name string) (provider.Provider, error) {
	c.providersMu.Lock()
	defer c.providersMu.Unlock()
	if p, ok := c.providers[name]; ok {
		return p, nil
	}
	if err := provider.Unavailable(name); err != nil {
		return nil, err
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
		entry, err := provider.NewEntry(name)
		if err != nil {
			return nil, err
		}
		p = entry
	}
	if c.providers == nil {
		c.providers = map[string]provider.Provider{}
	}
	c.providers[name] = p
	return p, nil
}

// cachedProvider is the client already made for a provider, or nil.
func (c *config) cachedProvider(name string) provider.Provider {
	c.providersMu.Lock()
	defer c.providersMu.Unlock()
	return c.providers[name]
}

// forgetProviders drops the cached clients drop reports true for, so they are made
// again on next use.
func (c *config) forgetProviders(drop func(name string) bool) {
	c.providersMu.Lock()
	defer c.providersMu.Unlock()
	for name := range c.providers {
		if drop(name) {
			delete(c.providers, name)
		}
	}
}

func (c *config) currentProvider() (provider.Provider, error) {
	if c.model.IsZero() {
		return nil, fmt.Errorf("no model is selected; pick one with /model, or start goat with -m provider:model")
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
	if configStartup.model != "" {
		candidates = append(candidates, candidate{"config.json", configStartup.model})
	}
	if raw := strings.TrimSpace(os.Getenv("OPENAI_MODEL")); raw != "" {
		candidates = append(candidates, candidate{"OPENAI_MODEL", "openai:" + strings.TrimPrefix(raw, "openai:")})
	}

	for _, c := range candidates {
		ref, err := provider.ResolveModel(c.raw)
		if err != nil {
			if c.source == "config.json" {
				// Its errors repeat the name, and a key may have been pasted there.
				warnf("ignoring the model in config.json: goat does not know which provider serves it; write it as provider:model")
			} else {
				warnf("ignoring the model from %s: %v", c.source, err)
			}
			continue
		}
		if unavailable := provider.Unavailable(ref.Provider); unavailable != nil {
			switch c.source {
			case "GOAT_MODEL":
				warnf("ignoring GOAT_MODEL=%s: %v", c.raw, unavailable)
			case "--model":
				warnf("ignoring --model %s: %v", c.raw, unavailable)
			case "config.json":
				warnf("ignoring the model in config.json: %v", unavailable)
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
	candidates = append(candidates, struct{ source, value string }{"config.json", configStartup.effort})
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

	cfg := &config{model: model, reasoningEffort: startupEffort(saved, opts.effort), autoCompactTokens: autoCompactTokens, planMode: opts.plan, codeMode: opts.codeMode || envFlag("GOAT_CODEMODE"), continueSaved: opts.continueSession}
	if opts.print && !opts.continueSession {
		// runOneShot gives the run a conversation of its own; the saved one is left as it is.
		activeConversationID = newConversationID()
		return cfg
	}
	restoreSavedConversation(cfg, saved)
	return cfg
}

// restoreSavedConversation sets up the conversation a start begins with. Unless -c
// asked to continue, that is a new one, with the saved one kept for /resume.
//
// With -c the saved conversation continues when it belongs to the active model's
// provider. Otherwise the previous conversation is tried, and if neither fits a new
// conversation starts with the saved one kept for /resume. Without a session file it
// falls back to the response ID in the history log, which is how OpenAI conversations
// were resumed before.
func restoreSavedConversation(cfg *config, saved *savedSession) {
	if saved != nil && cfg.model.IsZero() {
		// Without a key there is no model yet; the session files wait for one.
		activeConversationID = newConversationID()
		cfg.pendingSaved = saved
		return
	}
	if !cfg.continueSaved {
		startNewConversation(cfg, saved)
		return
	}

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

	if saved.Conversation.IsEmpty() && len(saved.PendingToolResults) == 0 {
		// A start that sent nothing leaves an empty session behind; -c means the last
		// conversation that had something in it.
		if previous, _ := loadSessionFile(previousSessionPath); previous != nil && !previous.Conversation.IsEmpty() && previous.Conversation.Fits(cfg.model) {
			adoptSavedConversation(cfg, previous)
			saveSession(cfg)
			return
		}
	}
	if saved.Conversation.Fits(cfg.model) {
		adoptSavedConversation(cfg, saved)
		return
	}

	// The saved conversation belongs to another provider, or to another of opencode-go's
	// APIs. If the previous one fits, swap the two; otherwise start fresh and keep the
	// saved one for /resume.
	previous, _ := loadSessionFile(previousSessionPath)
	if err := sessionSaver(previousSessionPath, *saved); err != nil {
		warnf("failed to keep the saved conversation: %v", err)
		return
	}
	if previous != nil && !previous.Conversation.IsEmpty() && previous.Conversation.Fits(cfg.model) {
		adoptSavedConversation(cfg, previous)
		saveSession(cfg)
		fmt.Println(formatNotice(fmt.Sprintf("Continuing the earlier %s conversation; the %s one is kept and /resume switches to it.", previous.Model, saved.Model)))
		return
	}

	startConversation(cfg, conversationState{})
	if err := provider.Unavailable(saved.Conversation.Provider); err == nil {
		fmt.Println(formatNotice(fmt.Sprintf("The saved %s conversation does not carry over to %s; it was kept, and /resume restores it.", saved.Model, cfg.model)))
	} else {
		fmt.Println(formatNotice(fmt.Sprintf("The saved %s conversation was kept; /resume restores it once %s can be used (%v).", saved.Model, saved.Conversation.Provider, err)))
	}
}

// startNewConversation begins a new conversation at startup. A saved conversation
// with anything in it moves to session.prev.json, where /resume finds it; an empty one
// is not worth keeping and would push out the conversation before it.
func startNewConversation(cfg *config, saved *savedSession) {
	if saved == nil || (saved.Conversation.IsEmpty() && len(saved.PendingToolResults) == 0) {
		activeConversationID = newConversationID()
		return
	}
	if err := sessionSaver(previousSessionPath, *saved); err != nil {
		// session.json is then the only copy; this run leaves it be, and -c continues it.
		warnf("failed to keep the saved conversation for /resume: %v; this run will not be saved, and goat -c continues the saved one", err)
		activeConversationID = newConversationID()
		cfg.ephemeral = true
		return
	}
	cfg.keptPrevious = true
	startConversation(cfg, conversationState{})
	recordHistory(historyEntry{Type: "reset", Provider: cfg.model.Provider})
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

// uiMu guards activeInputGuard and activeKeyCapture, which goroutines other than
// main may look at.
var uiMu sync.Mutex

// activeInputGuard is the guard of the command or turn currently running, if any.
var activeInputGuard *terminal.InputGuard

func currentInputGuard() *terminal.InputGuard {
	uiMu.Lock()
	defer uiMu.Unlock()
	return activeInputGuard
}

func setActiveInputGuard(guard *terminal.InputGuard) {
	uiMu.Lock()
	defer uiMu.Unlock()
	activeInputGuard = guard
}

func runWithInputGuard(cbreak bool, work func() error) error {
	guard, err := terminal.BeginInputGuard(cbreak)
	if err != nil {
		return err
	}
	setActiveInputGuard(guard)
	defer func() {
		setActiveInputGuard(nil)
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
	capture := currentKeyCapture()
	capture.pause()
	suspended := time.Now()
	guard := currentInputGuard()
	if err := guard.Suspend(); err != nil {
		warnf("failed to restore terminal echo: %v", err)
	}
	return func() {
		inputWaitTotal.Add(int64(time.Since(suspended)))
		if err := guard.Resume(); err != nil {
			warnf("failed to disable terminal echo: %v", err)
		}
		capture.resume()
		resumeSpinner()
	}
}

// inputWaitTotal is the total time spent waiting on the user's answers mid-turn, so
// a tool's elapsed time can leave out how long its approval took. It counts
// nanoseconds.
var inputWaitTotal atomic.Int64

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
	cfg.lastImages = takePromptImages(cfg, prompt)
	return runPromptWithImages(ctx, cfg, prompt, cfg.lastImages)
}

// runPromptWithImages runs one turn for prompt with the images attached to it.
func runPromptWithImages(ctx context.Context, cfg *config, prompt string, userImages []provider.Image) error {
	cfg.agents.sync(cfg)
	// Commands subagents asked to run since the last turn are asked about first.
	cfg.agents.serveApprovals(ctx, cfg)
	collectAgentNotices(cfg)

	p, err := cfg.currentProvider()
	if err != nil {
		return err
	}

	// Compaction is tried again between tool rounds, but after a failure, or a round
	// that is still too big right after compacting, not again this turn: each attempt
	// costs a summary request.
	midTurnCompaction, justCompacted := true, false
	// The prompt goes with the turn's first request, so it counts toward the size.
	if compacted, err := maybeAutoCompactConversation(ctx, cfg, "", prompt); err != nil {
		if ctx.Err() != nil {
			return err
		}
		midTurnCompaction = false
		warnf("%v; continuing with the full conversation", err)
	} else {
		justCompacted = compacted
	}

	// turnStarted is set once the model has answered this turn's first request; lastOwed
	// are the tool results the latest round produced. Recovery needs both, and after a
	// compaction partway through the turn the conversation's shape no longer shows them.
	turnStarted, lastOwed := false, []provider.ToolResult(nil)
	opts := turnOptions{
		model:       cfg.model.ID,
		effort:      cfg.reasoningEffort,
		planMode:    cfg.planMode,
		codeMode:    cfg.codeMode,
		vision:      provider.AcceptsImages(cfg.model),
		images:      userImages,
		contextNote: shellContextNote(cfg.pendingShell) + agentNoticesNote(cfg.agentNotices),
		// Save after every tool round and whatever the outcome, so a crash or Ctrl-C
		// never loses work the model already did.
		checkpoint: func(state conversationState) {
			turnStarted, lastOwed = true, state.pendingToolResults
			cfg.conversation = state
			cfg.pendingShell, cfg.agentNotices = nil, nil
			saveSession(cfg)
			// The footer under the input catches up with the turn: context used, commands
			// left running.
			if cfg.interactive && style.Enabled() {
				setTurnFooter(turnFooterParts(cfg, true))
			}
		},
		// A long tool loop can outgrow the threshold before the turn ends; checked after
		// each round's checkpoint, so cfg holds the round's state.
		compact: func(ctx context.Context) (provider.Conversation, bool) {
			if !midTurnCompaction {
				return provider.Conversation{}, false
			}
			if justCompacted {
				justCompacted = false
				if entries, err := loadConversationHistoryEntries(); err == nil {
					threshold, _ := autoCompactThreshold(cfg)
					if over, _ := shouldAutoCompactConversation(entries, threshold); over {
						midTurnCompaction = false
						warnf("the conversation is still over the auto-compact threshold right after compacting; not compacting again this turn")
						return provider.Conversation{}, false
					}
				}
			}
			compacted, err := maybeAutoCompactConversation(ctx, cfg, midTurnCompactionFocus, "")
			if err != nil {
				midTurnCompaction = false
				if ctx.Err() == nil {
					warnf("%v; continuing with the full conversation", err)
				}
				return provider.Conversation{}, false
			}
			justCompacted = compacted
			return cfg.conversation.conversation, compacted
		},
	}
	defer saveSession(cfg)
	if cfg.approver != nil {
		opts.approveBash = cfg.approver.approve
	}
	if cfg.agents != nil {
		opts.agents = cfg.agents
		opts.serveAgents = func(ctx context.Context) { cfg.agents.serveApprovals(ctx, cfg) }
		opts.takeNotices = cfg.agents.turnNotices
	}
	// Plan mode changes nothing, so there is nothing to check.
	if !cfg.planMode {
		// While a general subagent still works, the check waits for a later turn.
		opts.verify = cfg.agents.deferVerify(verifyGate(), &cfg.verifyPending)
		// The retry after compacting below is the same turn. A check put off by an
		// earlier turn runs when this one ends.
		opts.verified = &verifyState{changed: cfg.verifyPending && opts.verify != nil}
	}
	// Messages queued while the turn works go to the model with the next tool results.
	if cfg.interactive && style.Enabled() {
		opts.takeSteering = takeQueuedText
	}

	if len(cfg.agentNotices) > 0 {
		// The reports go before the prompt; the history keeps them too, so a summary of
		// the conversation, or its export, has what the model acted on.
		recordHistory(historyEntry{Type: "user", Content: strings.Join(cfg.agentNotices, "\n\n"), Provider: p.Name()})
	}
	sent := cfg.conversation.conversation
	next, err := sendPromptStream(ctx, p, prompt, cfg.conversation, opts)
	cfg.conversation = next
	if err == nil || conversationMoved(sent, next.conversation) {
		// The ! output went with this message, which the model has now seen. A request
		// that failed outright keeps it for /retry.
		cfg.pendingShell, cfg.agentNotices = nil, nil
	}
	if err == nil || !provider.IsThinkingBindingError(err) && !errors.Is(err, errContextExceeded) {
		return err
	}

	// The API no longer accepts this conversation's thinking blocks, or it outgrew the
	// model's window. Carry it over in a summary and try once more. A turn the model had
	// already started goes on from where it was instead of starting over.
	moved := turnStarted || conversationMoved(sent, next.conversation)
	owed := next.pendingToolResults
	// Steered messages and agents' reports the failed request carried go with the retry.
	pendingText := next.pendingText
	if len(owed) == 0 && turnStarted {
		// A request after compacting partway through carried them as text.
		owed = lastOwed
	}
	if errors.Is(err, errContextExceeded) && !moved && int64(len([]rune(prompt)))/charsPerTokenEstimate >= contextBudget(cfg) {
		// No summary makes room for a prompt that does not fit by itself.
		return fmt.Errorf("%w: the prompt itself is too big for this model; shorten it", err)
	}
	fmt.Fprintln(activityOut, formatNotice("The conversation could not be continued as-is; compacting it and retrying."))
	summaryConversation, summary, compactErr := compactConversation(ctx, p, cfg.model.ID, "")
	if compactErr != nil {
		return fmt.Errorf("%w (compacting it failed too: %v; use /clear)", err, compactErr)
	}
	applyCompactedConversation(cfg, summaryConversation, summary, "recovery")
	cfg.conversation.pendingText = pendingText
	if moved {
		// The note, any ! output and the prompt's images went with the turn's first
		// request. The retry carries the owed results as text, so their images go with it.
		opts.contextNote = ""
		opts.images = resultImages(owed)
	}
	sent = cfg.conversation.conversation
	next, err = sendPromptStream(ctx, p, recoveryPromptText(prompt, moved, owed), cfg.conversation, opts)
	cfg.conversation = next
	if err == nil || conversationMoved(sent, next.conversation) {
		cfg.pendingShell, cfg.agentNotices = nil, nil
	}
	if errors.Is(err, errContextExceeded) {
		return fmt.Errorf("%w even after compacting; shorten the prompt, or start over with /clear", err)
	}
	return err
}

// conversationMoved reports whether a response was added to the conversation, so the
// request before it reached the model.
func conversationMoved(before, after provider.Conversation) bool {
	return after.ResponseID != before.ResponseID || len(after.Messages) != len(before.Messages)
}

// handleInput runs one command or prompt. Ctrl-C cancels it instead of killing the goat.
func handleInput(cfg *config, text string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	shellCommand, afterShell, isShell := shellEscapeText(cfg, text)
	if isShell && afterShell != "" {
		// Only the first line is the command; the rest run after it.
		requeueLines(afterShell)
	}
	var cmd *cliCommand
	var args []string
	if !isShell {
		var err error
		if cmd, args, err = lookupCommand(text); err != nil {
			return err
		}
	}

	// On an interactive color terminal, what is typed while the work runs is queued.
	queueing := cfg.interactive && style.Enabled()
	started := time.Now()
	before, _ := loadConversationHistoryEntries()
	err := runWithInputGuard(queueing, func() error {
		if queueing {
			fmt.Print(bracketedPasteOn)
			defer fmt.Print(bracketedPasteOff)
			setTurnFooter(turnFooterParts(cfg, !isShell && cmd == nil))
			startKeyCapture(ctx, cfg.input)
			defer stopKeyCapture()
		}
		switch {
		case isShell:
			return runShellEscape(ctx, cfg, shellCommand)
		case cmd != nil:
			cfg.commandText = text
			// A command can change the model or modes the agents take from main.
			defer cfg.agents.sync(cfg)
			return cmd.callback(ctx, cfg, args)
		}
		return runPrompt(ctx, cfg, text)
	})
	if cmd == nil && !isShell && err == nil && style.Enabled() {
		if elapsed := time.Since(started); elapsed >= time.Second {
			stopSpinner()
			fmt.Fprintln(activityOut, "\n"+renderWorked(elapsed, turnCostText(before)))
		}
	}
	return err
}

// migrateLegacyState moves the .goat directory an older goat kept in the workspace to
// goat's state directory, and says so. A -p run says it on stderr, keeping stdout for
// the reply.
func migrateLegacyState(oneShot bool) {
	result, err := workspace.MigrateLegacyState()
	if err != nil {
		warnf("could not move %s out of the workspace: %v", workspace.StateDirName, err)
		return
	}
	if len(result.Moved) > 0 {
		message := fmt.Sprintf("Moved goat's files from %s to %s.", workspace.StateDirName, displayPath(result.Root))
		switch {
		case !oneShot:
			fmt.Println(formatNotice(message))
		case stderrColor():
			fmt.Fprintln(os.Stderr, formatNotice(message))
		default:
			fmt.Fprintln(os.Stderr, message)
		}
	}
	if len(result.Clashed) > 0 {
		warnf("%s still holds %s: %s already has files of those names, so they were left; remove them from %s once you no longer need them", workspace.StateDirName, strings.Join(result.Clashed, ", "), displayPath(result.Root), workspace.StateDirName)
	}
	for _, name := range slices.Sorted(maps.Keys(result.Failed)) {
		warnf("could not move %s/%s to %s: %v", workspace.StateDirName, name, displayPath(result.Root), result.Failed[name])
	}
}

// printWelcome shows the banner and the notices about the restored state.
func printWelcome(cfg *config) {
	printBanner(cfg)
	switch {
	case !cfg.conversation.conversation.IsEmpty():
		fmt.Println(formatNotice("Continuing the saved conversation."))
	case cfg.keptPrevious:
		fmt.Println(formatNotice("New conversation; /resume brings back the previous one."))
	}
	if sources := projectInstructionSources(); len(sources) > 0 {
		fmt.Println(formatNotice("Project instructions: " + strings.Join(sources, ", ") + " (sent with each new conversation)."))
	}
	if cfg.approver.autoApprove.Load() {
		message := "run_bash commands are auto-approved (GOAT_AUTO_APPROVE_BASH)."
		if style.Enabled() {
			message = formatWarning(message)
		}
		fmt.Println(message)
	}
}

// printBanner shows the welcome box with the current model, effort, directory and branch.
func printBanner(cfg *config) {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	git := ""
	if state, ok := readGitState(); ok {
		git = state.footerText()
	}
	fmt.Println(renderBanner(cfg.model.String(), string(cfg.reasoningEffort), shortenHome(cwd, home), git, terminalWidth()))
}

// printModelLine restates the banner's model row after the model or effort changed, so
// the current values are on screen next to the change. Plain output has no banner box
// and already names the change, so it gets nothing.
func printModelLine(cfg *config) {
	if style.Enabled() {
		fmt.Println(renderModelLine(cfg.model.String(), string(cfg.reasoningEffort), terminalWidth()))
	}
}

// footerParts is the status shown under the interactive prompt.
func footerParts(cfg *config) []string {
	parts := []string{cfg.model.String(), "effort " + string(cfg.reasoningEffort)}
	if cfg.planMode {
		parts = append(parts, "plan mode")
	}
	if cfg.codeMode {
		parts = append(parts, "code mode")
	}
	if running := backgroundFooterPart(); running != "" {
		parts = append(parts, running)
	}
	if pending := pendingShellFooterPart(cfg); pending != "" {
		parts = append(parts, pending)
	}
	if state, ok := readGitState(); ok {
		parts = append(parts, state.footerText())
	}
	if !cfg.conversation.conversation.IsEmpty() {
		if entries, err := loadConversationHistoryEntries(); err == nil {
			parts = append(parts, "~"+formatTokenCount(estimateConversationTokens(entries))+" ctx")
		}
	}
	if cfg.approver != nil && cfg.approver.autoApprove.Load() {
		parts = append(parts, "auto-approve on")
	}
	return append(parts, shortcutsFooterPart)
}

const shortcutsFooterPart = "? for shortcuts"

// turnFooterParts is the footer under the input while a turn runs: the prompt's,
// without the shortcuts hint, since "?" is just text there. A prompt's turn also leaves
// out the ! output waiting, which it is about to send or has sent.
func turnFooterParts(cfg *config, prompt bool) []string {
	pending := pendingShellFooterPart(cfg)
	var parts []string
	for _, part := range footerParts(cfg) {
		if (prompt && part == pending) || part == shortcutsFooterPart {
			continue
		}
		parts = append(parts, part)
	}
	return parts
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
	parts := footerParts(cfg)
	liveFooter := func() string { return renderFooter(withAgentsPart(parts, agentsFooterPart()), width) }
	footer := liveFooter()
	if text, ok, err := editPrompt(ctx, cfg.input, cfg.promptHistory, footer, width, sessionImages{cfg}, cfg.agents.promptEvents(liveFooter)); ok {
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

// keylessStartFails reports a start that cannot go on without a key: a one-shot run, or
// input that is not a terminal, where nobody can type /login.
func keylessStartFails(keyless, print, stdinTerminal bool) bool {
	return keyless && (print || !stdinTerminal)
}

// exitConfirmWindow is how soon a second Ctrl-C at the prompt has to follow the first
// to exit.
const exitConfirmWindow = 2 * time.Second

func main() {
	// Before the flags, so --model can name an entry's model. Entries also come before
	// the global .env, so it treats their key variables as credentials. config.json's
	// settings go into the environment before .env, which then cannot override them.
	configEntries, envWarnings, entriesErr := loadConfigFile()
	if entriesErr == nil {
		var entryWarnings []string
		entryWarnings, entriesErr = loadProviderEntries(configEntries)
		envWarnings = append(envWarnings, entryWarnings...)
	}
	opts, err := parseCLI(os.Args[1:], os.Stdout)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		if entriesErr != nil {
			// A model the broken file would have defined is not the real problem.
			err = entriesErr
		}
		for _, warning := range envWarnings {
			// Such as providers.json being unreadable, which may be why a model is unknown.
			fmt.Fprintln(os.Stderr, "[warn] "+warning)
		}
		fmt.Fprintln(os.Stderr, "goat: "+err.Error())
		os.Exit(2)
	}
	if opts.showVersion {
		fmt.Println("goat " + goatVersion())
		return
	}
	if entriesErr != nil {
		log.Fatal(entriesErr)
	}

	// Keys from auth.json before the global .env, which is then not used for the
	// providers auth.json supplies.
	// File tools stay out of the config directory, which a workspace such as the home
	// directory contains.
	workspace.ProtectDir(goatConfigDir(), "goat's config directory, which holds the user's keys")
	authWarnings, err := loadAuthFile()
	if err != nil {
		log.Fatal(err)
	}
	envWarnings = append(envWarnings, authWarnings...)
	// The files can hold any GOAT_ setting, so they load before the settings are read.
	globalWarnings, err := loadGlobalEnv()
	if err != nil {
		log.Fatal(err)
	}
	envWarnings = append(envWarnings, globalWarnings...)
	// Before the terminal is set up, so a password manager can prompt on it.
	envWarnings = append(envWarnings, runKeyCommands()...)
	// Nor into goat's own state, once the global .env, which can set XDG_STATE_HOME, is in.
	workspace.ProtectDir(workspace.StateHome(), "goat's state directory, with the history and undo snapshots")
	// Read at package initialization, before the files loaded.
	style.Detect()
	reducedMotion = envFlag("GOAT_REDUCED_MOTION")
	// Without a key, an interactive session still starts, so /login can add one.
	keyless := len(provider.AvailableNames()) == 0
	if keylessStartFails(keyless, opts.print, terminal.StdinIsTerminal()) {
		// A failed key command or an unreadable global file may be why.
		for _, warning := range envWarnings {
			fmt.Fprintln(os.Stderr, "[warn] "+warning)
		}
		log.Fatal(noAPIKeyMessage())
	}

	provider.Warnf = warnf
	workspace.Warnf = warnf
	terminal.Warnf = warnf
	applyTheme()
	for _, warning := range append(envWarnings, endpointNotices()...) {
		warnf("%s", warning)
	}
	// Before the session is read: an older goat kept it in the workspace.
	migrateLegacyState(opts.print)
	cfg := loadConfig(opts)
	cfg.input = newLineReader(os.Stdin)
	cfg.promptHistory = loadPromptHistory()
	cfg.interactive = terminal.StdinIsTerminal()
	cfg.approver = &bashApprover{input: cfg.input, interactive: cfg.interactive}
	cfg.approver.autoApprove.Store(opts.autoApprove || envFlag("GOAT_AUTO_APPROVE_BASH"))
	cfg.approver.sessionAutoApprove.Store(cfg.approver.autoApprove.Load())
	argumentCompletions = commandArgumentCompletions(cfg)
	projectInstructions = projectInstructionsText
	// Before the exit paths are set up, so every one of them stops the agents.
	startAgents(cfg, opts.print)
	stopBackgroundOnSignal()

	if opts.print {
		// A one-shot run never asks anything: stdin may hold the prompt itself.
		cfg.interactive = false
		cfg.approver.interactive = false
		cfg.oneShot = true
		code := runOneShot(cfg, opts)
		stopBackgroundOnExit(os.Stderr)
		os.Exit(code)
	}
	if opts.prompt != "" {
		// A prompt on the command line runs first, as if it had been queued.
		enqueue(opts.prompt)
	}

	printWelcome(cfg)
	if keyless {
		fmt.Println(formatWarning(keylessNotice()))
	}
	runInteractive(cfg)
}

// sayGoodbye stops what goat left running in the background and says goodbye.
func sayGoodbye() {
	stopBackgroundOnExit(os.Stdout)
	fmt.Println("Closing the Goat... Goodbye!")
}

// runInteractive is the prompt loop: it reads a prompt or command, runs it, and
// repeats until the user exits.
func runInteractive(cfg *config) {
	gap := true
	var lastInterrupt time.Time
	for {
		// Background agents that finished since the last turn are shown, and their
		// reports go with the next message; commands they asked to run are asked about.
		collectAgentNotices(cfg)
		serveIdleApprovals(cfg)
		// Messages queued during the last turn run before the prompt comes back.
		var err error
		text, queued := nextQueuedInput()
		if queued {
			fmt.Println("\n" + renderUserMessage(text, terminalWidth()))
		} else {
			text, err = readPrompt(cfg, gap)
		}
		gap = true
		if errors.Is(err, errUIEvent) {
			// A subagent asked or finished while the prompt waited: dealt with at the top
			// of the loop, then the prompt comes back with what was typed.
			gap = false
			continue
		}
		openedAgents := errors.Is(err, errOpenAgents)
		if openedAgents {
			text, err = agentsCommandLine, nil
		}
		if errors.Is(err, context.Canceled) {
			if time.Since(lastInterrupt) < exitConfirmWindow {
				sayGoodbye()
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
			sayGoodbye()
			return
		}
		if strings.TrimSpace(text) == "" {
			gap = false
			continue
		}

		err = handleInput(cfg, text)
		stopSpinner()
		if openedAgents {
			// The prompt left no line behind for Ctrl-G, so none goes above the next.
			gap = false
		}
		if err != nil {
			// Nothing queued is sent after a turn that was interrupted or failed; it
			// waits in the prompt instead.
			returnQueueToPrompt(cfg.input)
		}
		switch {
		case err == nil:
		case errors.Is(err, errExitRequested):
			sayGoodbye()
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
