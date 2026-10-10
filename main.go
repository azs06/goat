package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/joho/godotenv"

	"github.com/azs06/goat/internal/provider"
)

type config struct {
	model             provider.Model
	reasoningEffort   provider.Effort
	autoCompactTokens int64
	conversation      conversationState
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
	// lastModelChoices are the models the last /model listing numbered.
	lastModelChoices []provider.Model
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

// startupModel picks the first usable model in order: GOAT_MODEL, the saved session,
// OPENAI_MODEL, then a default for whichever provider has credentials. Candidates whose
// provider has no API key are skipped.
func startupModel(saved *savedSession) (provider.Model, string) {
	type candidate struct{ source, raw string }
	var candidates []candidate
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
			fmt.Fprintf(os.Stderr, "[warn] ignoring the model from %s: %v\n", c.source, err)
			continue
		}
		if !provider.Available(ref.Provider) {
			if c.source == "GOAT_MODEL" {
				fmt.Fprintf(os.Stderr, "[warn] ignoring GOAT_MODEL=%s: %v\n", c.raw, provider.MissingCredentialsError(ref.Provider))
			}
			continue
		}
		return ref, c.source
	}
	return provider.DefaultModel(), "default"
}

// startupEffort picks the effort in order: GOAT_EFFORT, the saved session,
// OPENAI_REASONING_EFFORT, then the default.
func startupEffort(saved *savedSession) provider.Effort {
	candidates := []struct{ source, value string }{{"GOAT_EFFORT", os.Getenv("GOAT_EFFORT")}}
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
			fmt.Fprintf(os.Stderr, "[warn] invalid effort from %s: %v\n", candidate.source, err)
			continue
		}
		return effort
	}
	return provider.DefaultEffort
}

func loadConfig() *config {
	saved, err := loadSessionFile(sessionPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] failed to load the saved session: %v\n", err)
	}

	model, _ := startupModel(saved)
	autoCompactTokens, err := parseAutoCompactTokens(os.Getenv("GOAT_AUTO_COMPACT_TOKENS"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] invalid GOAT_AUTO_COMPACT_TOKENS: %v; using the default\n", err)
		autoCompactTokens = 0
	}

	cfg := &config{model: model, reasoningEffort: startupEffort(saved), autoCompactTokens: autoCompactTokens}
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
				fmt.Fprintf(os.Stderr, "[warn] failed to load saved conversation context: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "[warn] failed to keep the saved conversation: %v\n", err)
		return
	}
	if previous != nil && !previous.Conversation.IsEmpty() && previous.Conversation.Provider == cfg.model.Provider {
		adoptSavedConversation(cfg, previous)
		saveSession(cfg)
		fmt.Printf("Continuing the earlier %s conversation; the %s one is kept and /resume switches to it.\n", cfg.model.Provider, saved.Conversation.Provider)
		return
	}

	startConversation(cfg, conversationState{})
	if provider.Available(saved.Conversation.Provider) {
		fmt.Printf("The saved %s conversation does not carry over to %s; it was kept, and /resume restores it.\n", saved.Model, cfg.model)
	} else {
		fmt.Printf("The saved %s conversation was kept; /resume restores it once %s.\n", saved.Model, provider.MissingCredentialsError(saved.Conversation.Provider))
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
var activeInputGuard *terminalInputGuard

func runWithInputGuard(work func() error) error {
	guard, err := beginTerminalInputGuard()
	if err != nil {
		return err
	}
	activeInputGuard = guard
	defer func() {
		activeInputGuard = nil
		if closeErr := guard.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "[warn] failed to restore terminal input state: %v\n", closeErr)
		}
	}()

	return work()
}

// suspendInputGuard turns terminal echo back on so the user can answer a question in
// the middle of a turn, and returns the function that turns it off again.
func suspendInputGuard() func() {
	guard := activeInputGuard
	if err := guard.suspend(); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] failed to restore terminal echo: %v\n", err)
	}
	return func() {
		if err := guard.resume(); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] failed to disable terminal echo: %v\n", err)
		}
	}
}

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
		fmt.Fprintf(os.Stderr, "[warn] %v; continuing with the full conversation\n", err)
	}

	// Save after every tool round and whatever the outcome, so a crash or Ctrl-C never
	// loses work the model already did.
	previousCheckpoint := turnCheckpoint
	turnCheckpoint = func(state conversationState) {
		cfg.conversation = state
		saveSession(cfg)
	}
	defer func() {
		turnCheckpoint = previousCheckpoint
		saveSession(cfg)
	}()

	next, err := sendPromptStream(ctx, p, prompt, cfg.conversation, cfg.model.ID, cfg.reasoningEffort)
	cfg.conversation = next
	if err == nil || !provider.IsThinkingBindingError(err) {
		return err
	}

	// The API no longer accepts this conversation's thinking blocks. Carry it over in a
	// summary and try once more.
	fmt.Println("The conversation could not be continued as-is; compacting it and retrying.")
	summaryConversation, summary, compactErr := compactConversation(ctx, p, cfg.model.ID, "")
	if compactErr != nil {
		return fmt.Errorf("%w (compacting it failed too: %v; use /clear)", err, compactErr)
	}
	applyCompactedConversation(cfg, summaryConversation, summary, "recovery")
	next, err = sendPromptStream(ctx, p, prompt, cfg.conversation, cfg.model.ID, cfg.reasoningEffort)
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

	return runWithInputGuard(func() error {
		if cmd != nil {
			return cmd.callback(ctx, cfg, args)
		}
		return runPrompt(ctx, cfg, text)
	})
}

func main() {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Fatalf("Error loading .env file: %v", err)
	}
	if len(provider.AvailableNames()) == 0 {
		log.Fatal("no API key found; set OPENAI_API_KEY, ANTHROPIC_API_KEY (or ANTHROPIC_AUTH_TOKEN) or DEEPSEEK_API_KEY, in the environment or a .env file")
	}

	cfg := loadConfig()
	cfg.input = newLineReader(os.Stdin)
	cfg.interactive = stdinIsTerminal()
	cfg.approver = &bashApprover{input: cfg.input, interactive: cfg.interactive, autoApprove: envFlag("GOAT_AUTO_APPROVE_BASH")}
	bashCommandApprover = cfg.approver.approve

	fmt.Println("Welcome to G.O.A.T agent")
	fmt.Printf("Model: %s (effort %s). Type /help for commands.\n", cfg.model, cfg.reasoningEffort)
	if !cfg.conversation.conversation.IsEmpty() {
		fmt.Println("Resumed saved conversation context.")
	}
	if cfg.approver.autoApprove {
		fmt.Println("run_bash commands are auto-approved (GOAT_AUTO_APPROVE_BASH).")
	}

	for {
		fmt.Print("G.O.A.T > ")
		text, err := cfg.input.ReadLine(context.Background())
		if err != nil {
			fmt.Println()
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "Error reading input: %v\n", err)
			}
			fmt.Println("Closing the Goat... Goodbye!")
			return
		}
		if strings.TrimSpace(text) == "" {
			continue
		}

		err = handleInput(cfg, text)
		switch {
		case err == nil:
		case errors.Is(err, errExitRequested):
			fmt.Println("Closing the Goat... Goodbye!")
			return
		case errors.Is(err, context.Canceled):
			fmt.Println("\nInterrupted.")
		default:
			fmt.Println("Error:", err)
		}
	}
}
