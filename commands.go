package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/azs06/goat/internal/provider"
)

var errExitRequested = errors.New("exit requested")

var commandWordPattern = regexp.MustCompile(`^/[a-z]+$`)

type cliCommand struct {
	name        string
	usage       string
	description string
	aliases     []string
	callback    func(ctx context.Context, c *config, args []string) error
}

// commands is filled in init because commandHelp reads it.
var (
	commands       map[string]*cliCommand
	commandAliases map[string]string
)

func init() {
	list := []*cliCommand{
		{name: "approve", usage: "/approve [on|off]", description: "Show or change whether run_bash commands run without asking", callback: commandApprove},
		{name: "clear", usage: "/clear", description: "Start a new conversation (the old one is kept for /resume)", aliases: []string{"new", "reset"}, callback: commandClear},
		{name: "compact", usage: "/compact [focus]", description: "Summarize the conversation into a fresh, smaller context", callback: commandCompact},
		{name: "effort", usage: "/effort [level]", description: "Show or set how much the model reasons", callback: commandEffort},
		{name: "exit", usage: "/exit", description: "Exit the Goat", aliases: []string{"quit"}, callback: commandExit},
		{name: "help", usage: "/help", description: "Display available commands", callback: commandHelp},
		{name: "history", usage: "/history", description: "Show the conversation history path", callback: commandHistory},
		{name: "model", usage: "/model [model|number|list]", description: "Pick the model; lists only providers with an API key", callback: commandModel},
		{name: "resume", usage: "/resume", description: "Swap back to the previous conversation", callback: commandResume},
		{name: "retry", usage: "/retry", description: "Send the last prompt again", callback: commandRetry},
		{name: "status", usage: "/status", description: "Show the model, providers, context size and settings", callback: commandStatus},
		{name: "tools", usage: "/tools", description: "List available tools", callback: commandTools},
		{name: "undo", usage: "/undo [preview|skip]", description: "Undo the latest file change (/undo skip gives up on one that cannot be restored)", callback: commandUndo},
	}

	commands = map[string]*cliCommand{}
	commandAliases = map[string]string{}
	for _, cmd := range list {
		commands[cmd.name] = cmd
		for _, alias := range cmd.aliases {
			commandAliases[alias] = cmd.name
		}
	}
}

func findCommand(name string) (*cliCommand, bool) {
	if canonical, ok := commandAliases[name]; ok {
		name = canonical
	}
	cmd, ok := commands[name]
	return cmd, ok
}

// bareWordCommands also run without a slash when typed alone. They are the commands
// goat had before slash commands; newer ones such as /clear, /model or /effort only run
// with the slash, so a one-word prompt like "clear" still reaches the model.
var bareWordCommands = map[string]bool{"compact": true, "exit": true, "quit": true, "help": true, "history": true, "reset": true, "resume": true, "status": true, "tools": true, "undo": true}

// lookupCommand returns the command a line of input asks for, or nil when the line is a
// prompt for the model. A command runs when it starts with a slash ("/undo"); the
// bare-word commands also run when they are the whole line ("undo"), so prompts such as
// "undo the refactor in foo.go" reach the model.
func lookupCommand(text string) (*cliCommand, []string, error) {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil, nil, nil
	}

	name := strings.ToLower(words[0])
	if strings.HasPrefix(name, "/") {
		if cmd, ok := findCommand(name[1:]); ok {
			return cmd, words[1:], nil
		}
		// "/tmp is full" is a prompt; only a lone unknown "/word" is a mistyped command.
		if len(words) == 1 && commandWordPattern.MatchString(name) {
			return nil, nil, fmt.Errorf("unknown command %s; type /help to list commands", words[0])
		}
		return nil, nil, nil
	}

	if len(words) == 1 && bareWordCommands[name] {
		if cmd, ok := findCommand(name); ok {
			return cmd, nil, nil
		}
	}
	return nil, nil, nil
}

func commandExit(ctx context.Context, c *config, args []string) error {
	return errExitRequested
}

func commandHelp(ctx context.Context, c *config, args []string) error {
	fmt.Println("Welcome to the Goat!")
	fmt.Println("Anything else you type is sent to the agent. Press Ctrl-C while it works to interrupt the turn.")
	fmt.Println("Commands:")

	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cmd := commands[name]
		line := fmt.Sprintf("  %-28s %s", cmd.usage, cmd.description)
		if len(cmd.aliases) > 0 {
			aliases := make([]string, len(cmd.aliases))
			for i, alias := range cmd.aliases {
				aliases[i] = "/" + alias
			}
			line += " (also " + strings.Join(aliases, ", ") + ")"
		}
		fmt.Println(line)
	}
	fmt.Println("help, exit, quit, status, compact, undo, tools, history, resume and reset also work without the slash when typed alone.")
	return nil
}

func commandClear(ctx context.Context, c *config, args []string) error {
	replaceConversation(c, conversationState{})
	recordHistory(historyEntry{Type: "reset", Provider: c.model.Provider})
	fmt.Println("Started a new conversation. /resume brings the previous one back.")
	return nil
}

// commandResume swaps the current conversation with the one kept in session.prev.json.
func commandResume(ctx context.Context, c *config, args []string) error {
	previous, err := loadSessionFile(previousSessionPath)
	if err != nil {
		return err
	}
	if previous == nil || previous.Conversation.IsEmpty() {
		return fmt.Errorf("there is no previous conversation to resume")
	}

	model, err := provider.ResolveModel(previous.Model)
	if err != nil {
		return fmt.Errorf("the previous conversation names an unknown model: %w", err)
	}
	if previous.Conversation.Provider != "" && previous.Conversation.Provider != model.Provider {
		return fmt.Errorf("the previous conversation is inconsistent (model %s, conversation from %s)", model, previous.Conversation.Provider)
	}
	if !provider.Available(model.Provider) {
		return fmt.Errorf("the previous conversation used %s: %w", model, provider.MissingCredentialsError(model.Provider))
	}

	if err := sessionSaver(previousSessionPath, sessionFromConfig(c)); err != nil {
		return err
	}
	c.model = model
	if effort, err := provider.ParseEffort(previous.Effort); err == nil && previous.Effort != "" {
		c.reasoningEffort = effort
	}
	adoptSavedConversation(c, previous)
	saveSession(c)

	fmt.Printf("Resumed the previous conversation on %s. /resume again switches back.\n", c.model)
	return nil
}

func commandTools(ctx context.Context, c *config, args []string) error {
	fmt.Println("Available tools:")
	for _, name := range availableToolNames() {
		fmt.Printf("- %s\n", name)
	}
	return nil
}

func commandUndo(ctx context.Context, c *config, args []string) error {
	undo := undoLastChange
	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "skip":
			undo = skipLastChange
		case "preview":
			undo = previewUndoLastChange
		default:
			return fmt.Errorf("unknown undo option %q; use /undo, /undo preview or /undo skip", args[0])
		}
	}

	message, err := undo()
	if err != nil {
		return err
	}
	fmt.Println(message)
	return nil
}

func commandCompact(ctx context.Context, c *config, args []string) error {
	provider, err := c.currentProvider()
	if err != nil {
		return err
	}

	next, summary, err := compactConversation(ctx, provider, c.model.ID, strings.Join(args, " "))
	if err != nil {
		return err
	}

	applyCompactedConversation(c, next, summary, "manual")
	fmt.Println("Conversation context compacted.")
	return nil
}

func commandHistory(ctx context.Context, c *config, args []string) error {
	resolvedPath, relPath, err := resolveStatePath(conversationHistoryPath())
	if err != nil {
		return err
	}

	if info, err := os.Stat(resolvedPath); err == nil {
		fmt.Printf("Conversation history: %s (%d bytes)\n", relPath, info.Size())
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	fmt.Printf("Conversation history: %s (not created yet)\n", relPath)
	return nil
}

func commandRetry(ctx context.Context, c *config, args []string) error {
	if strings.TrimSpace(c.lastPrompt) == "" {
		return fmt.Errorf("there is no earlier prompt to retry")
	}
	fmt.Printf("Retrying: %s\n", truncateForLog(c.lastPrompt, 120))
	return runPrompt(ctx, c, c.lastPrompt)
}

func commandApprove(ctx context.Context, c *config, args []string) error {
	if c.approver == nil {
		return fmt.Errorf("command approval is not available")
	}
	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "on", "yes", "true":
			c.approver.autoApprove = true
		case "off", "no", "false":
			c.approver.autoApprove = false
		default:
			return fmt.Errorf("use /approve on or /approve off")
		}
	}
	if c.approver.autoApprove {
		fmt.Println("run_bash commands run without asking (/approve off to ask again).")
	} else {
		fmt.Println("run_bash asks before every command (/approve on to stop asking for this session).")
	}
	return nil
}

func commandEffort(ctx context.Context, c *config, args []string) error {
	if len(args) > 0 {
		effort, err := provider.ParseEffort(args[0])
		if err != nil {
			return err
		}
		c.reasoningEffort = effort
		saveSession(c)
	}
	fmt.Printf("Reasoning effort: %s (sent to %s as %s). Levels: %s\n", c.reasoningEffort, c.model, effectiveEffortDescription(c), provider.JoinEfforts(provider.EffortLevels))
	return nil
}

// effectiveEffortDescription says what the provider will actually receive.
func effectiveEffortDescription(c *config) string {
	return provider.DescribeEffort(c.providers[c.model.Provider], c.model, c.reasoningEffort)
}

func commandStatus(ctx context.Context, c *config, args []string) error {
	fmt.Printf("Model:     %s\n", c.model)
	fmt.Printf("Effort:    %s (sent as %s)\n", c.reasoningEffort, effectiveEffortDescription(c))
	for _, name := range provider.Names {
		if source := provider.CredentialSource(name); source != "" {
			fmt.Printf("Provider:  %-9s available (%s)\n", name, source)
		} else {
			fmt.Printf("Provider:  %-9s unavailable (%s not set)\n", name, strings.Join(provider.CredentialVars[name], " or "))
		}
	}

	conversation := "empty"
	if conv := c.conversation.conversation; !conv.IsEmpty() {
		if len(conv.Messages) > 0 {
			conversation = fmt.Sprintf("%d messages", len(conv.Messages))
		} else {
			conversation = "continuing from " + conv.ResponseID
		}
	}
	if pending := len(c.conversation.pendingToolResults); pending > 0 {
		conversation += fmt.Sprintf(", %d tool result(s) waiting to be sent", pending)
	}
	fmt.Printf("Session:   %s\n", conversation)
	if entries, err := loadConversationHistoryEntries(); err == nil {
		fmt.Printf("Context:   about %d tokens; auto-compacts at %d\n", estimateConversationTokens(entries), autoCompactThreshold(c))
	}
	if c.approver != nil {
		fmt.Printf("run_bash:  %s\n", map[bool]string{true: "runs without asking", false: "asks before each command"}[c.approver.autoApprove])
	}
	if wd, err := os.Getwd(); err == nil {
		fmt.Printf("Workspace: %s\n", wd)
	}
	return nil
}

// commandModel lists the models of providers that have an API key and switches to the
// chosen one. With no argument on a terminal it asks for a choice.
func commandModel(ctx context.Context, c *config, args []string) error {
	if len(args) == 1 && strings.EqualFold(args[0], "list") {
		live := map[string][]string{}
		for _, name := range provider.AvailableNames() {
			provider, err := c.provider(name)
			if err != nil {
				continue
			}
			listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			models, err := provider.ListModels(listCtx)
			cancel()
			if err != nil {
				fmt.Fprintf(os.Stderr, "[warn] could not list %s models: %v\n", name, shortError(err))
			}
			live[name] = models
		}
		printModelChoices(c, provider.ModelChoices(c.model, live))
		return nil
	}
	if len(args) > 0 {
		return switchModel(ctx, c, strings.Join(args, " "))
	}

	printModelChoices(c, provider.ModelChoices(c.model, nil))
	if !c.interactive {
		fmt.Println("Use /model <number or model> to switch, or /model list for every available model.")
		return nil
	}

	answer, err := c.ask(ctx, "Select a model by number or name (Enter keeps the current one): ")
	if err != nil || answer == "" {
		fmt.Printf("Keeping %s.\n", c.model)
		return nil
	}
	return switchModel(ctx, c, answer)
}

func printModelChoices(c *config, choices []provider.Model) {
	c.lastModelChoices = choices
	if len(choices) == 0 {
		fmt.Println("No models are available: set OPENAI_API_KEY, ANTHROPIC_API_KEY or DEEPSEEK_API_KEY.")
		return
	}
	fmt.Println("Models (only providers with an API key are listed):")
	for i, choice := range choices {
		marker := " "
		if choice == c.model {
			marker = "*"
		}
		fmt.Printf("%s %2d. %s\n", marker, i+1, choice)
	}
}

// switchModel changes the model. Within a provider the conversation carries on. Across
// providers it is summarized and seeded into the new one; nothing changes unless that
// works.
func switchModel(ctx context.Context, c *config, raw string) error {
	target, err := pickModel(c, raw)
	if err != nil {
		return err
	}
	if target == c.model {
		fmt.Printf("Already using %s.\n", target)
		return nil
	}
	if !provider.Available(target.Provider) {
		return provider.MissingCredentialsError(target.Provider)
	}
	p, err := c.provider(target.Provider)
	if err != nil {
		return err
	}
	if err := provider.CheckDeepSeekModel(ctx, p, target.ID); err != nil {
		return err
	}

	if target.Provider == c.model.Provider {
		c.model = target
		saveSession(c)
		fmt.Printf("Switched to %s; the conversation continues.\n", target)
		return nil
	}

	if c.conversation.conversation.IsEmpty() && len(c.conversation.pendingToolResults) == 0 {
		c.model = target
		saveSession(c)
		fmt.Printf("Switched to %s.\n", target)
		return nil
	}

	fmt.Printf("Carrying the conversation over to %s...\n", target)
	next, summary, err := compactConversation(ctx, p, target.ID, "")
	if err != nil {
		return fmt.Errorf("could not carry the conversation over to %s, so the model was not changed: %w (use /clear first to switch without it)", target, err)
	}
	previous := c.model
	recordCompactionSummary(c, summary, "model switch")
	// The archive keeps the old model, so /resume returns to the full conversation.
	archiveSession(c)
	c.model = target
	startConversation(c, conversationState{conversation: next})
	recordCompactionSeed(c, next, summary)
	fmt.Printf("Switched to %s with a summary of the conversation. /resume brings back the full %s one.\n", target, previous)
	return nil
}

// pickModel resolves a number from the last listing, or a model name.
func pickModel(c *config, raw string) (provider.Model, error) {
	raw = strings.TrimSpace(raw)
	if number, err := strconv.Atoi(raw); err == nil {
		choices := c.lastModelChoices
		if len(choices) == 0 {
			choices = provider.ModelChoices(c.model, nil)
		}
		if number < 1 || number > len(choices) {
			return provider.Model{}, fmt.Errorf("choose a number between 1 and %d", len(choices))
		}
		return choices[number-1], nil
	}

	return provider.ResolveModel(raw)
}
