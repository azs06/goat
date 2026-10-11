package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/responses"

	"github.com/azs06/goat/internal/provider"
)

// fakeProvider is a scripted Provider for tests that do not care about the wire format.
type fakeProvider struct {
	name        string
	responses   []*provider.Response
	requests    []provider.Request
	completeErr error
	// completeFailures makes the first Complete calls fail with a retryable error.
	completeFailures int
	completeCalls    int
	summary          string
	seeded           []string
	listed           []string
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Send(ctx context.Context, req provider.Request, onText func(string)) (*provider.Response, bool, error) {
	f.requests = append(f.requests, req)
	if len(f.responses) == 0 {
		return nil, false, errors.New("no scripted response")
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	return resp, false, nil
}

var errFakeBusy = errors.New("busy")

func (f *fakeProvider) Complete(ctx context.Context, model, system, prompt string) (string, error) {
	f.completeCalls++
	if f.completeErr != nil {
		return "", f.completeErr
	}
	if f.completeCalls <= f.completeFailures {
		return "", errFakeBusy
	}
	return f.summary, nil
}

func (f *fakeProvider) Seed(ctx context.Context, model, summary string) (provider.Conversation, error) {
	f.seeded = append(f.seeded, model)
	return provider.Conversation{Provider: f.name, ResponseID: "seeded-" + model, Messages: []json.RawMessage{json.RawMessage(`{"seed":true}`)}}, nil
}

func (f *fakeProvider) RetryDelay(err error, attempt int) (time.Duration, bool) {
	return 0, errors.Is(err, errFakeBusy)
}

func (f *fakeProvider) ListModels(ctx context.Context) ([]string, error) { return f.listed, nil }

func setCredentials(t *testing.T, openAI, anthropic bool) {
	t.Helper()
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "DEEPSEEK_API_KEY", "GOAT_MODEL", "OPENAI_MODEL", "GOAT_EFFORT", "OPENAI_REASONING_EFFORT"} {
		t.Setenv(name, "")
	}
	if openAI {
		t.Setenv("OPENAI_API_KEY", "sk-test")
	}
	if anthropic {
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	}
}

func newModelTestConfig(t *testing.T, current provider.Model, providers ...*fakeProvider) *config {
	t.Helper()
	cfg := &config{model: current, reasoningEffort: provider.EffortMedium, providers: map[string]provider.Provider{}}
	for _, p := range providers {
		cfg.providers[p.name] = p
	}
	return cfg
}

func TestSwitchModelRequiresTheProvidersAPIKey(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, true, false)

	cfg := newModelTestConfig(t, provider.Model{Provider: provider.OpenAI, ID: provider.DefaultOpenAIModel}, &fakeProvider{name: provider.OpenAI})
	err := commandModel(context.Background(), cfg, []string{"claude-opus-5-5"})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected the missing key to be named, got %v", err)
	}
	if cfg.model.Provider != provider.OpenAI {
		t.Fatalf("expected the model to stay unchanged, got %v", cfg.model)
	}
}

func TestSwitchModelWithinAProviderKeepsTheConversation(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, true, false)

	cfg := newModelTestConfig(t, provider.Model{Provider: provider.OpenAI, ID: provider.DefaultOpenAIModel}, &fakeProvider{name: provider.OpenAI})
	cfg.conversation = conversationState{conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-1"}, pendingToolResults: []provider.ToolResult{{CallID: "c1"}}}

	if err := commandModel(context.Background(), cfg, []string{"openai:gpt-other"}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if cfg.model != (provider.Model{Provider: provider.OpenAI, ID: "gpt-other"}) || cfg.conversation.conversation.ResponseID != "resp-1" || len(cfg.conversation.pendingToolResults) != 1 {
		t.Fatalf("expected the conversation to carry on, got %v %+v", cfg.model, cfg.conversation)
	}

	saved, err := loadSessionFile(sessionPath)
	if err != nil || saved == nil || saved.Model != "openai:gpt-other" {
		t.Fatalf("expected the choice to be saved, got %+v (%v)", saved, err)
	}
}

func TestSwitchModelAcrossProvidersCarriesASummary(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, true, true)
	recordHistory(historyEntry{Type: "user", Content: "Refactor the parser."})

	anthropicFake := &fakeProvider{name: provider.Anthropic, summary: "Goal: refactor the parser."}
	cfg := newModelTestConfig(t, provider.Model{Provider: provider.OpenAI, ID: provider.DefaultOpenAIModel}, &fakeProvider{name: provider.OpenAI}, anthropicFake)
	cfg.conversation = conversationState{conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-1"}}

	if err := commandModel(context.Background(), cfg, []string{"claude-opus-5-5"}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if cfg.model != (provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"}) || cfg.conversation.conversation.Provider != provider.Anthropic {
		t.Fatalf("expected the seeded Anthropic conversation, got %v %+v", cfg.model, cfg.conversation)
	}
	if len(anthropicFake.seeded) != 1 || anthropicFake.seeded[0] != "claude-opus-5-5" {
		t.Fatalf("expected the new model to be seeded, got %v", anthropicFake.seeded)
	}

	previous, err := loadSessionFile(previousSessionPath)
	if err != nil || previous == nil || previous.Model != "openai:"+provider.DefaultOpenAIModel || previous.Conversation.ResponseID != "resp-1" {
		t.Fatalf("expected the OpenAI conversation to be kept for /resume, got %+v (%v)", previous, err)
	}

	if err := commandResume(context.Background(), cfg, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if cfg.model.Provider != provider.OpenAI || cfg.conversation.conversation.ResponseID != "resp-1" {
		t.Fatalf("expected /resume to bring the OpenAI conversation back, got %v %+v", cfg.model, cfg.conversation)
	}
}

func TestFailedProviderSwitchChangesNothing(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, true, true)
	recordHistory(historyEntry{Type: "user", Content: "Refactor the parser."})

	cfg := newModelTestConfig(t, provider.Model{Provider: provider.OpenAI, ID: provider.DefaultOpenAIModel}, &fakeProvider{name: provider.OpenAI}, &fakeProvider{name: provider.Anthropic, completeErr: errors.New("overloaded")})
	start := conversationState{conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-1"}}
	cfg.conversation = start

	if err := commandModel(context.Background(), cfg, []string{"claude-opus-5-5"}); err == nil {
		t.Fatal("expected the switch to fail")
	}
	if cfg.model.Provider != provider.OpenAI || cfg.conversation.conversation.ResponseID != "resp-1" {
		t.Fatalf("expected nothing to change, got %v %+v", cfg.model, cfg.conversation)
	}
}

func TestSwitchModelByNumberFromTheListing(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, false, true)

	cfg := newModelTestConfig(t, provider.Model{Provider: provider.Anthropic, ID: provider.DefaultAnthropicModel}, &fakeProvider{name: provider.Anthropic})
	if err := commandModel(context.Background(), cfg, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(cfg.lastModelChoices) < 2 {
		t.Fatalf("expected several choices, got %v", cfg.lastModelChoices)
	}
	want := cfg.lastModelChoices[1]
	if err := commandModel(context.Background(), cfg, []string{"2"}); err != nil {
		t.Fatalf("pick: %v", err)
	}
	if cfg.model != want {
		t.Fatalf("expected %v, got %v", want, cfg.model)
	}
	if err := commandModel(context.Background(), cfg, []string{"99"}); err == nil {
		t.Fatal("expected an out-of-range number to fail")
	}
}

func TestModelListIncludesLiveModels(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, false, true)

	cfg := newModelTestConfig(t, provider.Model{Provider: provider.Anthropic, ID: provider.DefaultAnthropicModel}, &fakeProvider{name: provider.Anthropic, listed: []string{"claude-opus-5-5-20261001", "claude-opus-4-8"}})
	if err := commandModel(context.Background(), cfg, []string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	listed := map[string]bool{}
	for _, choice := range cfg.lastModelChoices {
		listed[choice.ID] = true
	}
	if !listed["claude-opus-5-5-20261001"] || listed["claude-opus-4-8"] {
		t.Fatalf("expected the live 5.5 model and not the older one, got %v", cfg.lastModelChoices)
	}
}

func TestSessionRoundTripAndCorruptFile(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	cfg := &config{
		model:           provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"},
		reasoningEffort: provider.EffortXhigh,
		conversation: conversationState{
			conversation:       provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hi"}]}`)}},
			pendingToolResults: []provider.ToolResult{{CallID: "toolu_1", Output: "out", IsError: true}},
		},
	}
	saveSession(cfg)

	info, err := os.Stat(filepath.Join(statePath(t, ""), "session.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("expected a private session file, got %v (%v)", info.Mode().Perm(), err)
	}
	saved, err := loadSessionFile(sessionPath)
	if err != nil || saved == nil {
		t.Fatalf("load: %v", err)
	}
	if saved.Model != "anthropic:claude-opus-5-5" || saved.Effort != "xhigh" || len(saved.Conversation.Messages) != 1 || saved.PendingToolResults[0].CallID != "toolu_1" || saved.PromptFingerprint != promptFingerprint(false) {
		t.Fatalf("unexpected session %+v", saved)
	}

	os.WriteFile(filepath.Join(statePath(t, ""), "session.json"), []byte("{broken"), 0600)
	if saved, err := loadSessionFile(sessionPath); err != nil || saved != nil {
		t.Fatalf("expected a corrupt session to be set aside, got %+v (%v)", saved, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(statePath(t, ""), "session.json.corrupt-*")); len(matches) != 1 {
		t.Fatalf("expected the corrupt file to be kept aside, found %d", len(matches))
	}
}

func TestStartupPrecedence(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, true, true)

	saved := &savedSession{Version: sessionVersion, Model: "anthropic:claude-sonnet-5-5", Effort: "high"}
	if model, _ := startupModel(saved, ""); model != (provider.Model{Provider: provider.Anthropic, ID: "claude-sonnet-5-5"}) {
		t.Fatalf("expected the saved model, got %v", model)
	}
	t.Setenv("OPENAI_MODEL", "gpt-legacy")
	if model, _ := startupModel(saved, ""); model.ID != "claude-sonnet-5-5" {
		t.Fatalf("expected the saved model to beat OPENAI_MODEL, got %v", model)
	}
	if model, _ := startupModel(nil, ""); model != (provider.Model{Provider: provider.OpenAI, ID: "gpt-legacy"}) {
		t.Fatalf("expected OPENAI_MODEL without a session, got %v", model)
	}
	t.Setenv("GOAT_MODEL", "claude-haiku-5-5")
	if model, _ := startupModel(saved, ""); model != (provider.Model{Provider: provider.Anthropic, ID: "claude-haiku-5-5"}) {
		t.Fatalf("expected GOAT_MODEL to win, got %v", model)
	}

	t.Setenv("OPENAI_REASONING_EFFORT", "low")
	if effort := startupEffort(saved, ""); effort != provider.EffortHigh {
		t.Fatalf("expected the saved effort to beat OPENAI_REASONING_EFFORT, got %v", effort)
	}
	t.Setenv("GOAT_EFFORT", "max")
	if effort := startupEffort(saved, ""); effort != provider.EffortMax {
		t.Fatalf("expected GOAT_EFFORT to win, got %v", effort)
	}

	setCredentials(t, false, true)
	if model := provider.DefaultModel(); model != (provider.Model{Provider: provider.Anthropic, ID: provider.DefaultAnthropicModel}) {
		t.Fatalf("expected the Anthropic default with only an Anthropic key, got %v", model)
	}
}

func TestLoadConfigRestoresMatchingSessionAndSetsAsideOthers(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, true, true)

	session := savedSession{Version: sessionVersion, Model: "anthropic:claude-opus-5-5", Conversation: provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{}`)}}, PromptFingerprint: promptFingerprint(false)}
	if err := writeSessionFile(sessionPath, session); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := loadConfig(cliOptions{})
	if cfg.model.Provider != provider.Anthropic || len(cfg.conversation.conversation.Messages) != 1 || cfg.staleConversation {
		t.Fatalf("expected the saved conversation, got %v %+v stale=%v", cfg.model, cfg.conversation, cfg.staleConversation)
	}

	session.PromptFingerprint = promptFingerprint(true)
	writeSessionFile(sessionPath, session)
	if cfg := loadConfig(cliOptions{}); cfg.staleConversation {
		t.Fatal("a conversation built in code mode is not stale")
	}

	session.PromptFingerprint = "older-goat"
	writeSessionFile(sessionPath, session)
	if cfg := loadConfig(cliOptions{}); !cfg.staleConversation {
		t.Fatal("expected a conversation from a different prompt to be marked stale")
	}

	t.Setenv("GOAT_MODEL", "gpt-6-luna")
	cfg = loadConfig(cliOptions{})
	if cfg.model.Provider != provider.OpenAI || !cfg.conversation.conversation.IsEmpty() {
		t.Fatalf("expected a fresh OpenAI conversation, got %v %+v", cfg.model, cfg.conversation)
	}
	if previous, _ := loadSessionFile(previousSessionPath); previous == nil || previous.Model != "anthropic:claude-opus-5-5" {
		t.Fatalf("expected the Anthropic conversation to be kept for /resume, got %+v", previous)
	}
}

func TestLoadConfigFallsBackWhenSavedProviderHasNoKey(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, true, false)

	writeSessionFile(sessionPath, savedSession{Version: sessionVersion, Model: "anthropic:claude-opus-5-5"})
	if cfg := loadConfig(cliOptions{}); cfg.model != (provider.Model{Provider: provider.OpenAI, ID: provider.DefaultOpenAIModel}) {
		t.Fatalf("expected the OpenAI default, got %v", cfg.model)
	}
}

func TestStaleConversationIsCompactedBeforeUse(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	recordHistory(historyEntry{Type: "user", Content: "earlier work"})

	p := &fakeProvider{name: provider.Anthropic, summary: "Summary."}
	cfg := newModelTestConfig(t, provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"}, p)
	cfg.conversation = conversationState{conversation: provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{"old":true}`)}}}
	cfg.staleConversation = true

	compacted, err := maybeAutoCompactConversation(context.Background(), cfg)
	if err != nil || !compacted {
		t.Fatalf("expected compaction, got %v %v", compacted, err)
	}
	if cfg.staleConversation || string(cfg.conversation.conversation.Messages[0]) != `{"seed":true}` {
		t.Fatalf("expected a fresh seeded conversation, got %+v", cfg.conversation)
	}
}

func TestRunPromptSavesTheSessionAfterEveryToolRound(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	var saves []savedSession
	previousSaver := sessionSaver
	sessionSaver = func(path string, session savedSession) error {
		if path == sessionPath {
			saves = append(saves, session)
		}
		return nil
	}
	t.Cleanup(func() { sessionSaver = previousSaver })

	p := &fakeProvider{name: provider.Anthropic, responses: []*provider.Response{
		{ID: "r1", Stop: provider.StopToolUse, ToolCalls: []provider.ToolCall{{ID: "t1", Name: "read_dir", Arguments: `{"path":"."}`}}, Conversation: provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{"n":1}`)}}},
		{ID: "r2", Stop: provider.StopEndTurn, Text: "done", Conversation: provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{"n":2}`)}}},
	}}
	cfg := newModelTestConfig(t, provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"}, p)

	if err := runPrompt(context.Background(), cfg, "list"); err != nil {
		t.Fatalf("runPrompt: %v", err)
	}
	if len(saves) != 2 || len(saves[0].PendingToolResults) != 1 || string(saves[1].Conversation.Messages[0]) != `{"n":2}` {
		t.Fatalf("expected a checkpoint after the tool round and a final save, got %+v", saves)
	}
	if cfg.lastPrompt != "list" {
		t.Fatalf("expected the prompt to be remembered for /retry, got %q", cfg.lastPrompt)
	}
}

func TestOpenAIRefusalThroughTheLoopKeepsPendingResults(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	stream := func(ctx context.Context, params responses.ResponseNewParams, onText func(string)) (*responses.Response, bool, error) {
		var resp responses.Response
		json.Unmarshal([]byte(`{"id":"r-refused","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"No."}]}]}`), &resp)
		return &resp, false, nil
	}

	start := conversationState{conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-prev"}, pendingToolResults: []provider.ToolResult{{CallID: "call-1", Output: "x"}}}
	conv, err := sendPromptStream(context.Background(), provider.NewOpenAIWith(nil, stream), "hello", start, turnOptions{model: provider.DefaultOpenAIModel, effort: provider.EffortMedium})
	if err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("expected a refusal error, got %v", err)
	}
	if conv.conversation.ResponseID != "resp-prev" || len(conv.pendingToolResults) != 1 {
		t.Fatalf("expected the state to be unchanged, got %+v", conv)
	}
}

func TestResumedConversationFindsItsOwnHistory(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, true, false)

	cfg := newModelTestConfig(t, provider.Model{Provider: provider.OpenAI, ID: provider.DefaultOpenAIModel}, &fakeProvider{name: provider.OpenAI, summary: "s"})
	startConversation(cfg, conversationState{})
	recordHistory(historyEntry{Type: "user", Content: "conversation A work"})
	recordHistory(historyEntry{Type: "assistant", Content: "A reply", ResponseID: "resp-a", ContextTokens: 140000})
	cfg.conversation = conversationState{conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-a"}}

	if err := commandClear(context.Background(), cfg, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	recordHistory(historyEntry{Type: "user", Content: "conversation B work"})

	if err := commandResume(context.Background(), cfg, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	entries, _ := loadConversationHistoryEntries()
	if estimate := estimateConversationTokens(entries); estimate < 140000 {
		t.Fatalf("expected the resumed conversation's size to be known, got %d", estimate)
	}
	source := buildCompactionSource(entries)
	if !strings.Contains(source, "conversation A work") || strings.Contains(source, "conversation B work") {
		t.Fatalf("expected only conversation A in its summary source, got %q", source)
	}
}

func TestStartupSwapsInThePreviousConversationForTheProvider(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, true, true)

	anthropicSession := savedSession{Version: sessionVersion, ConversationID: "cA", Model: "anthropic:claude-opus-5-5", Conversation: provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{"a":1}`)}}}
	openAISession := savedSession{Version: sessionVersion, ConversationID: "cB", Model: "openai:gpt-6-luna", Conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-b"}}
	writeSessionFile(previousSessionPath, anthropicSession)
	writeSessionFile(sessionPath, openAISession)

	t.Setenv("GOAT_MODEL", "claude-opus-5-5")
	cfg := loadConfig(cliOptions{})
	if len(cfg.conversation.conversation.Messages) != 1 || activeConversationID != "cA" {
		t.Fatalf("expected the earlier Anthropic conversation to be restored, got %+v (%s)", cfg.conversation, activeConversationID)
	}
	previous, _ := loadSessionFile(previousSessionPath)
	if previous == nil || previous.Conversation.ResponseID != "resp-b" {
		t.Fatalf("expected the OpenAI conversation to be kept for /resume, got %+v", previous)
	}
}

func TestStartupMismatchStartsASeparateConversation(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, true, true)

	activeConversationID = "cA"
	recordHistory(historyEntry{Type: "user", Content: "SECRET-A anthropic work"})
	recordHistory(historyEntry{Type: "assistant", Content: "A reply", ContextTokens: 120000})
	activeConversationID = ""
	writeSessionFile(sessionPath, savedSession{Version: sessionVersion, ConversationID: "cA", Model: "anthropic:claude-opus-5-5", Conversation: provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{}`)}}})

	t.Setenv("GOAT_MODEL", "gpt-6-luna")
	loadConfig(cliOptions{})
	recordHistory(historyEntry{Type: "user", Content: "B prompt"})
	entries, _ := loadConversationHistoryEntries()
	if source := buildCompactionSource(entries); strings.Contains(source, "SECRET-A") {
		t.Fatalf("expected the new conversation not to include A, got %q", source)
	}
	if estimate := estimateConversationTokens(entries); estimate > 1000 {
		t.Fatalf("expected the new conversation to start small, got %d", estimate)
	}
}

func TestOpenAIConversationsAreNeverStale(t *testing.T) {
	saved := &savedSession{PromptFingerprint: "older", Conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-1"}}
	if conversationIsStale(saved) {
		t.Fatal("expected an OpenAI conversation to survive a prompt change")
	}
	saved.Conversation = provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{}`)}}
	if !conversationIsStale(saved) {
		t.Fatal("expected a transcript from an older prompt to be stale")
	}
}

func TestStartupSkipsModelsWithoutCredentials(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	setCredentials(t, false, true)
	t.Setenv("GOAT_MODEL", "gpt-6-luna")
	saved := &savedSession{Version: sessionVersion, Model: "anthropic:claude-sonnet-5-5"}
	if model, _ := startupModel(saved, ""); model != (provider.Model{Provider: provider.Anthropic, ID: "claude-sonnet-5-5"}) {
		t.Fatalf("expected the saved Anthropic model, got %v", model)
	}

	setCredentials(t, true, false)
	t.Setenv("OPENAI_MODEL", "gpt-5.1")
	if model, _ := startupModel(&savedSession{Version: sessionVersion, Model: "anthropic:claude-opus-5-5"}, ""); model != (provider.Model{Provider: provider.OpenAI, ID: "gpt-5.1"}) {
		t.Fatalf("expected OPENAI_MODEL after skipping the saved Anthropic model, got %v", model)
	}
}

func TestModelPickerRejectsUnknownWords(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, true, false)

	cfg := newModelTestConfig(t, provider.Model{Provider: provider.OpenAI, ID: provider.DefaultOpenAIModel}, &fakeProvider{name: provider.OpenAI})
	if err := commandModel(context.Background(), cfg, []string{"cancel"}); err == nil {
		t.Fatal("expected an unknown word to be rejected")
	}
	if cfg.model.ID != provider.DefaultOpenAIModel {
		t.Fatalf("expected the model to stay, got %v", cfg.model)
	}
	if saved, _ := loadSessionFile(sessionPath); saved != nil {
		t.Fatalf("expected nothing to be saved, got %+v", saved)
	}
}

func TestConversationWithoutIDKeepsItsHistoryAcrossClearAndResume(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, true, false)

	// A conversation from before entries had IDs.
	recordHistory(historyEntry{Type: "user", Content: "LEGACY-A work"})
	recordHistory(historyEntry{Type: "assistant", Content: "A reply", ResponseID: "resp-a", ContextTokens: 140000})
	cfg := newModelTestConfig(t, provider.Model{Provider: provider.OpenAI, ID: provider.DefaultOpenAIModel}, &fakeProvider{name: provider.OpenAI})
	cfg.conversation = conversationState{conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-a"}}

	if err := commandClear(context.Background(), cfg, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	recordHistory(historyEntry{Type: "user", Content: "NEW-B work"})
	if err := commandResume(context.Background(), cfg, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}

	entries, _ := loadConversationHistoryEntries()
	source := buildCompactionSource(entries)
	if !strings.Contains(source, "LEGACY-A work") || strings.Contains(source, "NEW-B work") {
		t.Fatalf("expected only the legacy conversation, got %q", source)
	}
	if estimate := estimateConversationTokens(entries); estimate < 140000 {
		t.Fatalf("expected the legacy conversation's size, got %d", estimate)
	}
}

func TestFreshStartGetsAConversationID(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, false, true)

	loadConfig(cliOptions{})
	if activeConversationID == "" {
		t.Fatal("expected a new conversation to get an ID")
	}
}

func TestSavingKeepsAStaleConversationStale(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, false, true)

	writeSessionFile(sessionPath, savedSession{Version: sessionVersion, ConversationID: "c1", Model: "anthropic:claude-opus-5-5", PromptFingerprint: "older", Conversation: provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{}`)}}})
	cfg := loadConfig(cliOptions{})
	if !cfg.staleConversation {
		t.Fatal("expected the conversation to be stale")
	}
	if err := commandEffort(context.Background(), cfg, []string{"high"}); err != nil {
		t.Fatalf("effort: %v", err)
	}
	if cfg := loadConfig(cliOptions{}); !cfg.staleConversation {
		t.Fatal("expected the conversation to stay stale after a save")
	}
}

func TestCompactionSummaryIsCountedOnce(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	p := &fakeProvider{name: provider.OpenAI, summary: "UNIQUESUMMARY", completeFailures: 1}
	cfg := newModelTestConfig(t, provider.Model{Provider: provider.OpenAI, ID: provider.DefaultOpenAIModel}, p)
	startConversation(cfg, conversationState{conversation: provider.Conversation{Provider: provider.OpenAI, ResponseID: "resp-1"}})
	recordHistory(historyEntry{Type: "user", Content: "work"})

	previousSleeper := streamRetrySleeper
	streamRetrySleeper = func(ctx context.Context, delay time.Duration) error { return nil }
	t.Cleanup(func() { streamRetrySleeper = previousSleeper })

	if err := commandCompact(context.Background(), cfg, nil); err != nil {
		t.Fatalf("expected compaction to retry a transient failure, got %v", err)
	}
	if p.completeCalls != 2 {
		t.Fatalf("expected one retry, got %d call(s)", p.completeCalls)
	}
	entries, _ := loadConversationHistoryEntries()
	if count := strings.Count(buildCompactionSource(entries), "UNIQUESUMMARY"); count != 1 {
		t.Fatalf("expected the summary once in the new conversation, got %d", count)
	}
}

func TestAdoptingAnEmptyConversationUsesTheCurrentPrompt(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	cfg := &config{model: provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"}}
	adoptSavedConversation(cfg, &savedSession{ConversationID: "c1", PromptFingerprint: "older-goat"})
	cfg.conversation.conversation = provider.Conversation{Provider: provider.Anthropic, Messages: []json.RawMessage{json.RawMessage(`{}`)}}
	saved := sessionFromConfig(cfg)
	if conversationIsStale(&saved) {
		t.Fatal("expected a conversation built after the upgrade not to be stale")
	}
}

func TestLegacyResumeUsesTheResponsesOwnHistory(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	setCredentials(t, true, false)

	recordHistory(historyEntry{Type: "user", Content: "LEGACY"})
	recordHistory(historyEntry{Type: "assistant", Content: "legacy answer", ResponseID: "resp-old", ContextTokens: 500})
	activeConversationID = "c1"
	recordHistory(historyEntry{Type: "reset"})
	recordHistory(historyEntry{Type: "user", Content: "NEW"})
	recordHistory(historyEntry{Type: "assistant", Content: "new answer", ResponseID: "resp-new", ContextTokens: 120000, Provider: provider.OpenAI})
	activeConversationID = ""

	cfg := loadConfig(cliOptions{})
	if cfg.conversation.conversation.ResponseID != "resp-new" || activeConversationID != "c1" {
		t.Fatalf("expected to continue resp-new in its own scope, got %q in %q", cfg.conversation.conversation.ResponseID, activeConversationID)
	}
	entries, _ := loadConversationHistoryEntries()
	if estimate := estimateConversationTokens(entries); estimate < 120000 {
		t.Fatalf("expected the continued conversation's size, got %d", estimate)
	}
}
