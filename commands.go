package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/tools"
	"github.com/azs06/goat/internal/workspace"

	"github.com/azs06/goat/internal/style"
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
		{name: "ps", usage: "/ps [stop n|all]", description: "List the commands goat left running in the background, such as a dev server, and stop them", aliases: []string{"jobs"}, callback: commandProcesses},
		{name: "plan", usage: "/plan [on|off]", description: "Toggle plan mode: goat reads and plans but changes nothing", callback: commandPlan},
		{name: "codemode", usage: "/codemode [on|off]", description: "Toggle code mode: goat can batch tool calls in one run_code script", callback: commandCodeMode},
		{name: "diff", usage: "/diff [staged]", description: "Show the uncommitted changes in the workspace's git repository", callback: commandDiff},
		{name: "copy", usage: "/copy", description: "Copy the last reply to the clipboard", callback: commandCopy},
		{name: "export", usage: "/export [file]", description: "Write the conversation to a Markdown file", callback: commandExport},
		{name: "paste", usage: "/paste", description: "Attach the image on the clipboard to your next message (ctrl-v at the prompt)", callback: commandPaste},
		{name: "output", usage: "/output [n]", description: "Show a tool call's full output (n=1 is the latest)", callback: commandOutput},
		{name: "compact", usage: "/compact [focus]", description: "Summarize the conversation into a fresh, smaller context", callback: commandCompact},
		{name: "effort", usage: "/effort [level]", description: "Pick how much the model reasons, or set a level", callback: commandEffort},
		{name: "exit", usage: "/exit", description: "Exit the Goat", aliases: []string{"quit"}, callback: commandExit},
		{name: "help", usage: "/help", description: "List the commands and keys", callback: commandHelp},
		{name: "history", usage: "/history", description: "Show the conversation history path", callback: commandHistory},
		{name: "login", usage: "/login [provider]", description: "Save a provider's API key to auth.json and start using it", aliases: []string{"auth"}, callback: commandLogin},
		{name: "model", usage: "/model [name|list]", description: "Pick the model, or switch to one by name", callback: commandModel},
		{name: "resume", usage: "/resume", description: "Swap back to the previous conversation", callback: commandResume},
		{name: "retry", usage: "/retry", description: "Send the last prompt again", callback: commandRetry},
		{name: "status", usage: "/status", description: "Show the model, providers, context size and settings", callback: commandStatus},
		{name: "theme", usage: "/theme [name]", description: "Pick the theme, or switch to a built-in one such as one-dark (saved in config.json)", callback: commandTheme},
		{name: "tools", usage: "/tools", description: "List available tools", callback: commandTools},
		{name: "undo", usage: "/undo [preview|skip]", description: "Undo the latest file change (/undo skip gives up on one that cannot be restored)", callback: commandUndo},
		{name: "verify", usage: "/verify [command|run|off]", description: "Show or set the command goat runs to check a turn's changes (saved in config.json)", callback: commandVerify},
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
	width := terminalWidth()
	paragraph := func(text string) string {
		if !style.Enabled() {
			return text
		}
		return style.Wrap(style.Muted.Render(text), width-1, "", "")
	}
	fmt.Println(paragraph("Type a request and press Enter; anything that is not a command goes to the agent. Start a line with ! to run it in the shell instead, such as !git status; its output goes to the agent with your next message. Ctrl-C interrupts a turn."))
	fmt.Println()
	fmt.Println(renderHelpTable(commands))
	if style.Enabled() {
		fmt.Println(style.Strong.Render("Keys"))
		fmt.Println(strings.Join(renderShortcuts(width), "\n"))
	}
	fmt.Println()
	fmt.Println(paragraph("help, exit, quit, status, compact, undo, tools, history, resume and reset also work without the slash when typed alone."))
	return nil
}

func commandClear(ctx context.Context, c *config, args []string) error {
	if err := c.refuseWhileAgentsRun(); err != nil {
		return err
	}
	replaceConversation(c, conversationState{})
	c.pendingShell, c.agentNotices = nil, nil
	recordHistory(historyEntry{Type: "reset", Provider: c.model.Provider})
	fmt.Println(formatSuccess("Started a new conversation. /resume brings the previous one back."))
	// A new conversation starts under a fresh banner with the current model and effort.
	if style.Enabled() {
		fmt.Println()
		printBanner(c)
	}
	return nil
}

// commandResume swaps the current conversation with the one kept in session.prev.json.
func commandResume(ctx context.Context, c *config, args []string) error {
	if c.ephemeral {
		// Swapping would write over session.prev.json, which may hold the latest
		// conversation, while the one resumed here is never saved.
		return fmt.Errorf("/resume is not available in a run that is not saved; start goat with -c, or without -p")
	}
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
	if !previous.Conversation.Fits(model) {
		return fmt.Errorf("the previous conversation cannot continue on %s: its api in config.json has changed since", model.Provider)
	}
	if err := provider.Unavailable(model.Provider); err != nil {
		return fmt.Errorf("the previous conversation used %s: %w", model, err)
	}

	if err := sessionSaver(previousSessionPath, sessionFromConfig(c)); err != nil {
		return err
	}
	c.model = model
	effortNote := ""
	if effort, err := provider.ParseEffort(previous.Effort); err == nil && previous.Effort != "" {
		if effort != c.reasoningEffort {
			effortNote = " at effort " + string(effort)
		}
		c.reasoningEffort = effort
	}
	adoptSavedConversation(c, previous)
	c.pendingShell, c.agentNotices = nil, nil
	saveSession(c)

	fmt.Println(formatSuccess(fmt.Sprintf("Resumed the previous conversation on %s%s. /resume again switches back.", c.model, effortNote)))
	printModelLine(c)
	return nil
}

func commandTools(ctx context.Context, c *config, args []string) error {
	fmt.Println(style.Strong.Render(fmt.Sprintf("Available tools (%d)", len(tools.CodeModeSpecs)+len(tools.AgentSpecs))))
	fmt.Println()
	fmt.Println(renderToolList())
	return nil
}

func commandUndo(ctx context.Context, c *config, args []string) error {
	undo := workspace.UndoLastChange
	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "skip":
			undo = workspace.SkipLastChange
		case "preview":
			undo = workspace.PreviewUndoLastChange
		default:
			return fmt.Errorf("unknown undo option %q; use /undo, /undo preview or /undo skip", args[0])
		}
	}

	message, err := undo()
	if err != nil {
		return err
	}
	fmt.Println(formatSuccess(message))
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
	fmt.Println(formatSuccess("Conversation context compacted."))
	return nil
}

func commandHistory(ctx context.Context, c *config, args []string) error {
	resolvedPath, relPath, err := workspace.ResolveStatePath(conversationHistoryPath())
	if err != nil {
		return err
	}

	if info, err := os.Stat(resolvedPath); err == nil {
		fmt.Println(renderKeyValues([][2]string{{"Conversation history:", fmt.Sprintf("%s %s", displayPath(relPath), style.Muted.Render(fmt.Sprintf("(%d bytes)", info.Size())))}}))
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	fmt.Println(renderKeyValues([][2]string{{"Conversation history:", displayPath(relPath) + " " + style.Muted.Render("(not created yet)")}}))
	return nil
}

func commandRetry(ctx context.Context, c *config, args []string) error {
	if strings.TrimSpace(c.lastPrompt) == "" {
		return fmt.Errorf("there is no earlier prompt to retry")
	}
	fmt.Println(formatNotice("Retrying: " + truncateForLog(c.lastPrompt, 120)))
	// The images that went with it go again.
	return runPromptWithImages(ctx, c, c.lastPrompt, c.lastImages)
}

func commandApprove(ctx context.Context, c *config, args []string) error {
	if c.approver == nil {
		return fmt.Errorf("command approval is not available")
	}
	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "on", "yes", "true":
			c.approver.autoApprove.Store(true)
		case "off", "no", "false":
			c.approver.autoApprove.Store(false)
		default:
			return fmt.Errorf("use /approve on or /approve off")
		}
	}
	if c.approver.autoApprove.Load() {
		message := "run_bash commands run without asking "
		if style.Enabled() {
			message = formatWarning(message)
		}
		fmt.Println(message + style.Muted.Render("(/approve off to ask again)."))
	} else {
		fmt.Println(formatSuccess("run_bash asks before every command ") + style.Muted.Render("(/approve on to stop asking for this session)."))
	}
	return nil
}

// effortHints describe each effort level in the picker and the command menu.
var effortHints = map[provider.Effort]string{
	provider.EffortNone:    "no reasoning; quickest replies",
	provider.EffortMinimal: "barely any reasoning",
	provider.EffortLow:     "light reasoning; fast and cheap",
	provider.EffortMedium:  "balanced; the default",
	provider.EffortHigh:    "thorough; better for hard changes",
	provider.EffortXhigh:   "very thorough; slower",
	provider.EffortMax:     "as much as the model allows; slowest",
}

func effortOptions(c *config) []selectOption {
	options := make([]selectOption, 0, len(provider.EffortLevels))
	for _, level := range provider.EffortLevels {
		hint := effortHints[level]
		if sent := provider.DescribeEffort(c.cachedProvider(c.model.Provider), c.model, level); sent != string(level) {
			hint += " · sent as " + sent
		}
		options = append(options, selectOption{label: string(level), hint: hint, current: level == c.reasoningEffort})
	}
	return options
}

func commandEffort(ctx context.Context, c *config, args []string) error {
	if len(args) == 0 {
		chosen, err := selectOptionInteractive(ctx, c, "Reasoning effort for "+c.model.String(), effortOptions(c))
		switch {
		case errors.Is(err, errSelectorUnavailable):
		case err != nil:
			return err
		case chosen < 0:
			fmt.Println(formatNotice(fmt.Sprintf("Keeping effort %s.", c.reasoningEffort)))
			return nil
		default:
			args = []string{string(provider.EffortLevels[chosen])}
		}
	}
	sent := func() string {
		if description := effectiveEffortDescription(c); description != string(c.reasoningEffort) {
			return style.Muted.Render(" (sent to " + c.model.String() + " as " + description + ")")
		}
		return ""
	}
	if len(args) > 0 {
		effort, err := provider.ParseEffort(args[0])
		if err != nil {
			return err
		}
		c.reasoningEffort = effort
		saveSession(c)
		fmt.Println(formatSuccess("Effort set to "+string(effort)+".") + sent())
		printModelLine(c)
		return nil
	}
	fmt.Println("Reasoning effort: " + style.Highlight.Render(string(c.reasoningEffort)) + sent())
	fmt.Println(style.Muted.Render("Levels: " + provider.JoinEfforts(provider.EffortLevels) + ". Use /effort <level> to change it."))
	return nil
}

// effortStatusText is the effort, with what the model receives when that differs.
func effortStatusText(c *config) string {
	if sent := effectiveEffortDescription(c); sent != string(c.reasoningEffort) {
		return string(c.reasoningEffort) + style.Muted.Render(" (sent as "+sent+")")
	}
	return string(c.reasoningEffort)
}

// effectiveEffortDescription says what the provider will actually receive.
func effectiveEffortDescription(c *config) string {
	return provider.DescribeEffort(c.cachedProvider(c.model.Provider), c.model, c.reasoningEffort)
}

func commandStatus(ctx context.Context, c *config, args []string) error {
	rows := [][2]string{
		{"Model:", style.Highlight.Render(c.model.String())},
		{"Effort:", effortStatusText(c)},
		{"Images:", imagesStatusText(c.model)},
	}
	width := 9
	for _, name := range provider.Names {
		if !provider.IsPreset(name) || provider.Available(name) {
			width = max(width, len(name))
		}
	}
	for _, name := range provider.Names {
		origin := credentialOrigin(name)
		entry, isEntry := provider.LookupEntry(name)
		if entry.Preset && !provider.Available(name) {
			// Presets without a key would bury the rows that matter; /login lists them.
			continue
		}
		switch err := provider.Unavailable(name); {
		case err != nil && isEntry:
			rows = append(rows, [2]string{"Provider:", style.Muted.Render(style.PadRight(name, width) + " unavailable (" + err.Error() + ")")})
		case origin == "" && !isEntry && provider.NamedInAuth(name):
			rows = append(rows, [2]string{"Provider:", style.Muted.Render(style.PadRight(name, width) + " unavailable (" + err.Error() + ")")})
		case origin == "" && !isEntry:
			rows = append(rows, [2]string{"Provider:", style.Muted.Render(style.PadRight(name, width) + " unavailable (" + strings.Join(provider.CredentialVars[name], " or ") + " not set)")})
		case err != nil:
			rows = append(rows, [2]string{"Provider:", style.Muted.Render(style.PadRight(name, width) + " unavailable (" + err.Error() + ")")})
		default:
			endpoint, _ := provider.ResolveEndpoint(name)
			// An entry without key_env or key_command may still have a key in auth.json.
			keyless := origin == ""
			if keyless {
				origin = "no key"
			}
			if endpoint.Custom {
				origin += "; requests go to " + endpoint.Origin()
				if !keyless {
					origin += unencryptedNote(endpoint)
				}
			}
			rows = append(rows, [2]string{"Provider:", style.PadRight(name, width) + " " + style.Success.Render("available") + style.Muted.Render(" ("+origin+")")})
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
	rows = append(rows, [2]string{"Session:", conversation})
	if entries, err := loadConversationHistoryEntries(); err == nil {
		threshold, why := autoCompactThreshold(c)
		rows = append(rows, [2]string{"Context:", fmt.Sprintf("about %d tokens; auto-compacts at %d (%s)", estimateConversationTokens(entries), threshold, why)})
		if cost, responses, known := usageCost(historyEntriesSinceLastReset(entries)); responses > 0 {
			text := formatCost(cost) + " for this conversation"
			if !known {
				text += style.Muted.Render(" (some responses came from models without a price; see prices.json in the readme)")
			}
			rows = append(rows, [2]string{"Cost:", text})
		}
		if usage := conversationCacheUsage(entries); usage.responses > 0 {
			cache := fmt.Sprintf("%d%% of %d input tokens read from cache over %d response(s)", usage.read*100/usage.promptTokens, usage.promptTokens, usage.responses)
			if usage.written > 0 {
				cache += fmt.Sprintf("; %d written", usage.written)
			}
			rows = append(rows, [2]string{"Cache:", cache})
		}
	}
	if sources := projectInstructionSources(); len(sources) > 0 {
		rows = append(rows, [2]string{"Instructions:", strings.Join(sources, ", ")})
	}
	if c.planMode {
		rows = append(rows, [2]string{"Plan mode:", "on (changes are refused)"})
	}
	if c.codeMode {
		rows = append(rows, [2]string{"Code mode:", "on (tool calls can be batched in run_code)"})
	}
	if c.approver != nil {
		rows = append(rows, [2]string{"run_bash:", map[bool]string{true: style.Warning.Render("runs without asking"), false: "asks before each command"}[c.approver.autoApprove.Load()]})
	}
	if command, source := verifyCommand(); command != "" {
		rows = append(rows, [2]string{"Verify:", escapeControlChars(command) + style.Muted.Render(" ("+source+")")})
	}
	if wd, err := os.Getwd(); err == nil {
		home, _ := os.UserHomeDir()
		rows = append(rows, [2]string{"Workspace:", shortenHome(wd, home)})
	}
	if state, ok := readGitState(); ok {
		rows = append(rows, [2]string{"Git:", state.footerText()})
	}
	fmt.Println(renderKeyValues(rows))
	return nil
}

// commandModel lists the models of providers that have an API key and switches to the
// chosen one. With no argument on a terminal it asks for a choice.
func commandModel(ctx context.Context, c *config, args []string) error {
	if len(provider.AvailableNames()) == 0 {
		fmt.Println(formatWarning(keylessNotice()))
		return nil
	}
	if len(args) == 1 && strings.EqualFold(args[0], "list") {
		printModelChoices(c, provider.ModelChoices(c.model, liveModels(ctx, c)))
		return nil
	}
	if len(args) > 0 {
		return switchModel(ctx, c, strings.Join(args, " "))
	}

	if picked, err := pickModelInteractive(ctx, c, provider.ModelChoices(c.model, nil), true); !errors.Is(err, errSelectorUnavailable) {
		if err != nil || picked.IsZero() {
			if err == nil {
				fmt.Println(formatNotice(fmt.Sprintf("Keeping %s.", c.model)))
			}
			return err
		}
		return switchModel(ctx, c, picked.String())
	}

	printModelChoices(c, provider.ModelChoices(c.model, nil))
	if !c.interactive {
		fmt.Println(style.Muted.Render("Use /model <number or model> to switch, or /model list for every available model."))
		return nil
	}

	answer, err := c.ask(ctx, "Select a model by number or name (Enter keeps the current one): ")
	if err != nil || answer == "" {
		fmt.Println(formatNotice(fmt.Sprintf("Keeping %s.", c.model)))
		return nil
	}
	return switchModel(ctx, c, answer)
}

func printModelChoices(c *config, choices []provider.Model) {
	c.lastModelChoices = choices
	if len(choices) == 0 && len(provider.AvailableNames()) > 0 {
		fmt.Println(style.Warning.Render("No models to offer: " + strings.Join(provider.AvailableNames(), ", ") + " list none; use /model list, or /model provider:model."))
		return
	}
	if len(choices) == 0 {
		fmt.Println(style.Warning.Render("No models are available: " + strings.TrimPrefix(strings.TrimPrefix(noAPIKeyMessage(), "no API key found; "), "no provider can be used; ") + "."))
		return
	}
	fmt.Println(style.Strong.Render("Models") + style.Muted.Render(" (only providers with an API key are listed):"))
	fmt.Println(renderModelChoices(choices, c.model))
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
		fmt.Println(formatNotice(fmt.Sprintf("Already using %s.", target)))
		return nil
	}
	if err := provider.Unavailable(target.Provider); err != nil {
		return err
	}
	p, err := c.provider(target.Provider)
	if err != nil {
		return err
	}
	if err := provider.CheckModel(ctx, p, target.ID); err != nil {
		return err
	}

	if c.model.IsZero() {
		// The first model, after a start without a key: the saved session is restored
		// as a start with this model would have.
		c.model = target
		if saved := c.pendingSaved; saved != nil {
			c.pendingSaved = nil
			restoreSavedConversation(c, saved)
		}
		saveSession(c)
		fmt.Println(formatSuccess(fmt.Sprintf("Switched to %s.", target)))
		if c.keptPrevious {
			fmt.Println(formatNotice("New conversation; /resume brings back the previous one."))
		}
		printModelLine(c)
		return nil
	}

	// opencode-go speaks different APIs to different models; a switch between them
	// carries the conversation over like a switch of provider.
	if target.Provider == c.model.Provider && c.conversation.conversation.Fits(target) {
		c.model = target
		saveSession(c)
		fmt.Println(formatSuccess(fmt.Sprintf("Switched to %s; the conversation continues.", target)))
		printModelLine(c)
		return nil
	}

	if c.conversation.conversation.IsEmpty() && len(c.conversation.pendingToolResults) == 0 {
		c.model = target
		saveSession(c)
		fmt.Println(formatSuccess(fmt.Sprintf("Switched to %s.", target)))
		printModelLine(c)
		return nil
	}

	fmt.Println(formatNotice(fmt.Sprintf("Carrying the conversation over to %s...", target)))
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
	fmt.Println(formatSuccess(fmt.Sprintf("Switched to %s with a summary of the conversation. /resume brings back the full %s one.", target, previous)))
	printModelLine(c)
	return nil
}

// pickModel resolves a number from the last listing, or a model name.
// commandArgumentCompletions suggests values for the first argument of /effort, /model,
// /approve and /undo in the command menu.
func commandArgumentCompletions(c *config) func(command, partial string) []completion {
	return func(command, partial string) []completion {
		var items []completion
		add := func(value, description string) {
			if strings.HasPrefix(strings.ToLower(value), strings.ToLower(partial)) {
				items = append(items, completion{display: value, description: description, text: "/" + command + " " + value, argument: true})
			}
		}
		switch command {
		case "effort":
			for _, option := range effortOptions(c) {
				description := option.hint
				if option.current {
					description = "current · " + description
				}
				add(option.label, description)
			}
		case "model":
			for _, choice := range provider.ModelChoices(c.model, nil) {
				description := choice.Provider
				if hint := modelHints[choice.ID]; hint != "" {
					description += " · " + hint
				}
				if choice == c.model {
					description = "current · " + description
				}
				// A bare ID that would resolve elsewhere, such as an entry's gpt-oss:20b
				// or a model two entries list, keeps its provider.
				value := choice.ID
				if resolved, err := provider.ResolveModel(value); err != nil || resolved != choice {
					value = choice.String()
				}
				if strings.HasPrefix(strings.ToLower(choice.ID), strings.ToLower(partial)) || strings.HasPrefix(strings.ToLower(value), strings.ToLower(partial)) {
					items = append(items, completion{display: value, description: description, text: "/model " + value, argument: true})
				}
			}
			add("list", "every model the providers report")
		case "login":
			for _, name := range provider.Names {
				description := loginStatus(name)
				if label := provider.Label(name); label != name {
					description = label + " · " + description
				}
				add(name, description)
			}
		case "approve":
			add("on", "run commands without asking")
			add("off", "ask before each command")
		case "plan":
			add("on", "read and plan only")
			add("off", "allow changes again")
		case "codemode":
			add("on", "batch tool calls in run_code scripts")
			add("off", "call tools one at a time")
		case "undo":
			add("preview", "show what would be restored")
			add("skip", "give up on a change that cannot be restored")
		case "theme":
			for _, name := range style.BuiltinNames() {
				add(name, themeHints[name])
			}
		}
		return items
	}
}

// themeHints describe the built-in themes.
var themeHints = map[string]string{
	"default":  "black and white",
	"one-dark": "colors after Atom's One Dark",
}

// modelHints describe the models the picker offers by default.
var modelHints = map[string]string{
	"gpt-6-luna":        "OpenAI's default",
	"claude-opus-5-5":   "strong all-rounder; Anthropic's default",
	"claude-fable-5-1":  "most capable Claude; slower and pricier",
	"claude-sonnet-5-5": "fast everyday coding",
	"claude-haiku-5-5":  "fastest and cheapest Claude",
	"deepseek-v4-pro":   "DeepSeek's strongest model",
	"deepseek-flash":    "fast and cheap",
}

// moreModelsLabel is the picker entry that lists every model the providers report.
const moreModelsLabel = "Show every available model…"

// pickModelInteractive lets the user choose from choices with the arrow keys. It
// returns the zero model when they cancel, and errSelectorUnavailable when there is
// no interactive terminal. offerMore adds an entry that fetches the full model lists.
func pickModelInteractive(ctx context.Context, c *config, choices []provider.Model, offerMore bool) (provider.Model, error) {
	options := make([]selectOption, 0, len(choices)+1)
	for _, choice := range choices {
		options = append(options, selectOption{label: choice.ID, hint: modelHints[choice.ID], group: choice.Provider, current: choice == c.model})
	}
	if offerMore {
		options = append(options, selectOption{label: moreModelsLabel, hint: "asks each provider for its list"})
	}
	chosen, err := selectOptionInteractive(ctx, c, "Switch model", options)
	if err != nil || chosen < 0 {
		return provider.Model{}, err
	}
	if chosen < len(choices) {
		return choices[chosen], nil
	}
	return pickModelInteractive(ctx, c, provider.ModelChoices(c.model, liveModels(ctx, c)), false)
}

// liveModels asks every provider with credentials for its models.
func liveModels(ctx context.Context, c *config) map[string][]string {
	live := map[string][]string{}
	startSpinner("Listing models")
	defer stopSpinner()
	for _, name := range provider.AvailableNames() {
		if entry, ok := provider.LookupEntry(name); ok && len(entry.Models) > 0 {
			// The picker offers only the models the entry lists.
			continue
		}
		provider, err := c.provider(name)
		if err != nil {
			continue
		}
		listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		models, err := provider.ListModels(listCtx)
		cancel()
		if err != nil {
			warnf("could not list %s models: %v", name, shortError(err))
		}
		live[name] = models
	}
	return live
}

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
