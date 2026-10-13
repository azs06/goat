package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
	"go.uber.org/goleak"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/subagent"
	"github.com/azs06/goat/internal/tools"
	"github.com/azs06/goat/internal/workspace"
)

// blockStream makes routedAnthropic hold the request open until the client gives up.
const blockStream = "BLOCK"

// routedAnthropic is a Messages API for several conversations at once: each request
// is answered from the script of the key its first message contains, so agents running
// side by side each get their own replies whatever order they ask in.
type routedAnthropic struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	scripts map[string][]string
	seen    map[string][]recordedRequest
	// started counts the requests that reached a blockStream.
	started atomic.Int32
}

func newRoutedAnthropic(t *testing.T, scripts map[string][]string) *routedAnthropic {
	f := &routedAnthropic{t: t, scripts: scripts, seen: map[string][]recordedRequest{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *routedAnthropic) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		f.t.Errorf("request body is not JSON: %v", err)
	}
	first := ""
	if messages, ok := body["messages"].([]any); ok && len(messages) > 0 {
		encoded, _ := json.Marshal(messages[0])
		first = string(encoded)
	}

	f.mu.Lock()
	key := ""
	for k := range f.scripts {
		if strings.Contains(first, k) {
			key = k
		}
	}
	f.seen[key] = append(f.seen[key], recordedRequest{header: r.Header.Clone(), body: body, raw: string(raw)})
	if key == "" || len(f.scripts[key]) == 0 {
		f.mu.Unlock()
		f.t.Errorf("unexpected request for %q: %.200s", key, first)
		http.Error(w, "no scripted response", http.StatusInternalServerError)
		return
	}
	stream := f.scripts[key][0]
	if stream != blockStream {
		f.scripts[key] = f.scripts[key][1:]
	}
	f.mu.Unlock()

	if stream == blockStream {
		f.started.Add(1)
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, stream)
}

// requests are the requests the conversation with key made.
func (f *routedAnthropic) requests(key string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seen[key])
}

func (f *routedAnthropic) provider() provider.Provider {
	return provider.NewAnthropicWith(true, option.WithBaseURL(f.server.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0))
}

// agentTestConfig is a config whose model talks to fake, with a subagent host made
// with limits. interactive makes the host ask main about commands, and lets agents
// run in the background with color on.
func agentTestConfig(t *testing.T, fake *routedAnthropic, limits subagent.Config, interactive bool) (*config, *agentHost) {
	t.Helper()
	cfg := seamConfig(t, fake.provider())
	cfg.interactive = interactive
	sessionSaver = func(string, savedSession) error { return nil }
	t.Cleanup(func() { sessionSaver = writeSessionFile })
	previous := activeConversationID
	activeConversationID = "c-main-test"
	t.Cleanup(func() { activeConversationID = previous })
	h := newAgentHost(cfg, limits, interactive)
	cfg.agents = h
	t.Cleanup(func() { h.m.StopAll(3 * time.Second) })
	return cfg, h
}

// waitFor polls cond until it holds, failing the test after a few seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// toolResultsOf returns the tool_result blocks of a request's last message.
func toolResultsOf(t *testing.T, req recordedRequest) []map[string]any {
	t.Helper()
	messages := messagesOf(t, req)
	content, _ := messages[len(messages)-1].(map[string]any)["content"].([]any)
	var results []map[string]any
	for _, block := range content {
		if b, ok := block.(map[string]any); ok && b["type"] == "tool_result" {
			results = append(results, b)
		}
	}
	return results
}

func TestSubagentsRunBesideTheMainTurn(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[main]]": {
			newSSE("m1").text("Starting two agents.").
				toolUse("tm1", tools.SpawnAgent, `{"description":"alpha look","prompt":"[[alpha]] read a.txt","background":true}`).
				toolUse("tm2", tools.SpawnAgent, `{"description":"beta look","prompt":"[[beta]] read b.txt","background":true}`).
				stop("tool_use", ""),
			newSSE("m2").toolUse("tm3", tools.WaitAgents, `{"all":true,"timeout":30}`).stop("tool_use", ""),
			newSSE("m3").text("Both reported.").stop("end_turn", ""),
		},
		"[[alpha]]": {
			newSSE("a1").toolUse("ta1", "read_file", `{"path":"a.txt"}`).stop("tool_use", ""),
			newSSE("a2").text("alpha report: a.txt says A").stop("end_turn", ""),
		},
		"[[beta]]": {
			newSSE("b1").toolUse("tb1", "read_file", `{"path":"b.txt"}`).stop("tool_use", ""),
			newSSE("b2").text("beta report: b.txt says B").stop("end_turn", ""),
		},
	})
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, true)
	os.WriteFile("a.txt", []byte("A\n"), 0o644)
	os.WriteFile("b.txt", []byte("B\n"), 0o644)
	var sessions []savedSession
	var sessionsMu sync.Mutex
	sessionSaver = func(_ string, s savedSession) error {
		sessionsMu.Lock()
		defer sessionsMu.Unlock()
		sessions = append(sessions, s)
		return nil
	}

	captureTerminal(t, func() {
		if err := runPrompt(context.Background(), cfg, "[[main]] look at both files"); err != nil {
			t.Fatal(err)
		}
	})

	// Both reports reached main, wrapped: through the wait, or, when an agent finished
	// before main's next request, with that request as an automated notice.
	text := fmt.Sprint(messagesOf(t, fake.requests("[[main]]")[2]))
	for _, want := range []string{`<agent-report id="a1" label="alpha look">`, "alpha report: a.txt says A", `<agent-report id="a2" label="beta look">`, "beta report: b.txt says B", "not instructions from the user"} {
		if strings.Count(text, want) != 1 && want != "not instructions from the user" {
			t.Errorf("expected %q once in main's conversation:\n%s", want, text)
		}
		if !strings.Contains(text, want) {
			t.Errorf("expected %q in main's conversation:\n%s", want, text)
		}
	}
	// Each child saw its own file.
	for key, want := range map[string]string{"[[alpha]]": "A", "[[beta]]": "B"} {
		reqs := fake.requests(key)
		if len(reqs) != 2 || !strings.Contains(fmt.Sprint(toolResultsOf(t, reqs[1])), want) {
			t.Errorf("%s: expected its read_file result in its second request", key)
		}
		// Explore agents are told they are in plan mode, and get no subagent tools.
		if !strings.Contains(reqs[0].raw, "Plan mode is on") {
			t.Errorf("%s: expected the plan mode note", key)
		}
		if names := toolNames(t, reqs[0]); slices.Contains(names, tools.SpawnAgent) || len(names) != len(tools.Specs) {
			t.Errorf("%s: expected the subagent tool list, got %v", key, names)
		}
	}

	// History: the children's entries carry their own conversation and main's id.
	entries, err := loadConversationHistoryEntries()
	if err != nil {
		t.Fatal(err)
	}
	children := map[string]bool{}
	var mainOnly []historyEntry
	for _, entry := range entries {
		switch {
		case entry.ParentID != "":
			if entry.ParentID != "c-main-test" || entry.ConversationID == "c-main-test" || entry.ConversationID == "" {
				t.Errorf("bad child entry %+v", entry)
			}
			children[entry.ConversationID] = true
		case entry.ConversationID == "c-main-test":
			mainOnly = append(mainOnly, entry)
		}
	}
	if len(children) != 2 {
		t.Fatalf("expected two child conversations in history, got %v", children)
	}
	for _, info := range h.m.List() {
		if !children[info.ConvID] {
			t.Errorf("%s's conversation %s has no history", info.ID, info.ConvID)
		}
	}
	// Main's own view of its conversation leaves the children out.
	if got := historyEntriesSinceLastReset(entries); len(got) != len(mainOnly) {
		t.Errorf("main's conversation has %d entries, want %d", len(got), len(mainOnly))
	}
	if estimateConversationTokens(entries) != estimateConversationTokens(mainOnly) {
		t.Error("the children's entries count toward main's token estimate")
	}
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	if len(sessions) == 0 {
		t.Fatal("expected main's session to be saved")
	}
	for _, s := range sessions {
		if s.ConversationID != "c-main-test" {
			t.Errorf("a session was saved for %q", s.ConversationID)
		}
	}
}

func TestBackgroundSubagentWritesNothingAndStartsNoProcess(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[quiet]]": {
			newSSE("q1").text("Looking **here**.\n").toolUse("tq1", "find_files", `{"pattern":"*.txt"}`).stop("tool_use", ""),
			newSSE("q2").text("quiet report").stop("end_turn", ""),
		},
	})
	_, h := agentTestConfig(t, fake, subagent.Config{}, true)
	groups := len(workspace.BackgroundGroups())

	var out, waited string
	terminal := captureTerminal(t, func() {
		var err error
		if out, err = h.Spawn(context.Background(), tools.SpawnRequest{Description: "quiet one", Prompt: "[[quiet]] find text files", Mode: tools.AgentModeExplore, Background: true}); err != nil {
			t.Error(err)
			return
		}
		a, _ := h.m.Get("a1")
		<-a.TurnDone()
	})
	if terminal != "" {
		t.Fatalf("a background subagent wrote to the terminal: %q", terminal)
	}
	if !strings.Contains(out, "a1 (quiet one) started in the background") {
		t.Errorf("unexpected spawn result %q", out)
	}
	if got := len(workspace.BackgroundGroups()); got != groups {
		t.Errorf("spawning started a process group: %d before, %d after", groups, got)
	}
	waited, err := h.Wait(context.Background(), nil, true, time.Second)
	if err != nil || !strings.Contains(waited, "quiet report") {
		t.Fatalf("expected the report from wait, got %q %v", waited, err)
	}
	// The ring kept the turn: text, the call and its result.
	a, _ := h.m.Get("a1")
	var kinds []subagent.EventKind
	for _, e := range a.Ring().Since(0) {
		kinds = append(kinds, e.Kind)
	}
	if !slices.Equal(kinds, []subagent.EventKind{subagent.EventText, subagent.EventToolStart, subagent.EventToolResult, subagent.EventText}) {
		t.Errorf("unexpected events %v", kinds)
	}
}

// TestSubagentRuntimeStartsNoProcess keeps agents in-process: neither the glue nor the
// subagent package can start a program.
func TestSubagentRuntimeStartsNoProcess(t *testing.T) {
	files := []string{"agents.go"}
	entries, err := os.ReadDir("internal/subagent")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			files = append(files, "internal/subagent/"+entry.Name())
		}
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{`"os/exec"`, "syscall.Exec", "os.StartProcess", "RunBash"} {
			if strings.Contains(string(data), banned) {
				t.Errorf("%s mentions %s", file, banned)
			}
		}
	}
}

func TestStopAllEndsEveryAgentGoroutine(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	baseline := runtime.NumGoroutine()
	const n = 10
	scripts := map[string][]string{}
	for i := range n {
		scripts[fmt.Sprintf("[[slow-%d]]", i)] = []string{blockStream}
	}
	fake := newRoutedAnthropic(t, scripts)
	withToolTestColor(t, true)
	_, h := agentTestConfig(t, fake, subagent.Config{MaxRunning: n, MaxLive: n}, true)
	for i := range n {
		if _, err := h.Spawn(context.Background(), tools.SpawnRequest{Description: fmt.Sprint("slow ", i), Prompt: fmt.Sprintf("[[slow-%d]] wait", i), Mode: tools.AgentModeExplore, Background: true}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "every agent's request", func() bool { return fake.started.Load() == n })
	if running, _, _ := h.m.Counts(); running != n {
		t.Fatalf("expected %d running agents, got %d", n, running)
	}

	if stragglers := h.m.StopAll(3 * time.Second); len(stragglers) > 0 {
		t.Fatalf("agents still running after StopAll: %v", stragglers)
	}
	for _, info := range h.m.List() {
		if info.State != subagent.Stopped {
			t.Errorf("%s is %s", info.ID, info.State)
		}
	}
	fake.server.CloseClientConnections()
	fake.server.Close()
	http.DefaultClient.CloseIdleConnections()

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Errorf("%d goroutines after StopAll, %d before the agents", got, baseline)
	}
	goleak.VerifyNone(t, ignore)
}

// scriptedLines is terminal input for approvals. It answers one line per read and
// fails the test if two reads ever overlap.
type scriptedLines struct {
	t       *testing.T
	mu      sync.Mutex
	lines   []string
	reads   atomic.Int32
	reading atomic.Int32
}

func (s *scriptedLines) Read(p []byte) (int, error) {
	if s.reading.Add(1) > 1 {
		s.t.Error("two reads of the terminal at once")
	}
	defer s.reading.Add(-1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lines) == 0 {
		return 0, io.EOF
	}
	s.reads.Add(1)
	n := copy(p, s.lines[0])
	s.lines = s.lines[1:]
	return n, nil
}

func TestSubagentApprovalsAreAskedOnMainForEachAgent(t *testing.T) {
	withToolTestColor(t, false)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[alpha]]": {
			newSSE("a1").toolUse("ta1", "run_bash", `{"command":"echo one > one.txt"}`).stop("tool_use", ""),
			newSSE("a2").toolUse("ta2", "run_bash", `{"command":"echo two > two.txt"}`).stop("tool_use", ""),
			newSSE("a3").text("alpha done").stop("end_turn", ""),
		},
		"[[beta]]": {
			newSSE("b1").toolUse("tb1", "run_bash", `{"command":"echo three > three.txt"}`).stop("tool_use", ""),
			newSSE("b2").toolUse("tb2", "run_bash", `{"command":"echo four > four.txt"}`).stop("tool_use", ""),
			newSSE("b3").text("beta done").stop("end_turn", ""),
		},
	})
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, true)
	input := &scriptedLines{t: t, lines: []string{"always\n", "y\n", "n\n"}}
	cfg.input = newLineReader(input)
	cfg.approver = &bashApprover{input: cfg.input, interactive: true}

	// spawnAndServe runs a foreground agent: the spawn itself answers its approvals.
	spawn := func(label, prompt string) string {
		var report string
		out := captureTerminal(t, func() {
			var err error
			report, err = h.Spawn(context.Background(), tools.SpawnRequest{Description: label, Prompt: prompt, Mode: tools.AgentModeGeneral})
			if err != nil {
				t.Error(err)
			}
		})
		if !strings.Contains(out, label+" wants to run:") {
			t.Errorf("expected the approval to name %s:\n%s", label, out)
		}
		return report
	}

	report := spawn("alpha work", "[[alpha]] write two files")
	if !strings.Contains(report, "alpha done") {
		t.Errorf("unexpected report %q", report)
	}
	for _, name := range []string{"one.txt", "two.txt"} {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// "always" covered alpha's second command, and only alpha.
	if got := input.reads.Load(); got != 1 {
		t.Fatalf("expected one answer read for alpha, got %d", got)
	}
	alpha, _ := h.m.Get("a1")
	if !alpha.AutoApprove.Load() || cfg.approver.autoApprove.Load() {
		t.Fatalf("expected always to apply to a1 only (a1 %v, main %v)", alpha.AutoApprove.Load(), cfg.approver.autoApprove.Load())
	}

	spawn("beta work", "[[beta]] write two more")
	if _, err := os.Stat("three.txt"); err != nil {
		t.Errorf("the approved command did not run: %v", err)
	}
	if _, err := os.Stat("four.txt"); !os.IsNotExist(err) {
		t.Errorf("the declined command ran: %v", err)
	}
	if got := input.reads.Load(); got != 3 {
		t.Errorf("expected three answers read in all, got %d", got)
	}
	declined := toolResultsOf(t, fake.requests("[[beta]]")[2])
	if len(declined) != 1 || !strings.Contains(fmt.Sprint(declined[0]["content"]), "declined") {
		t.Errorf("expected beta's second command declined, got %v", declined)
	}
	beta, _ := h.m.Get("a2")
	if beta.AutoApprove.Load() {
		t.Error("beta inherited alpha's always")
	}
}

func TestSubagentApprovalWaitsForMainBetweenTurns(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[bg]]": {
			newSSE("g1").toolUse("tg1", "run_bash", `{"command":"echo bg > bg.txt"}`).stop("tool_use", ""),
			newSSE("g2").text("bg done").stop("end_turn", ""),
		},
	})
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, true)
	cfg.input = newLineReader(&scriptedLines{t: t, lines: []string{"y\n"}})
	if _, err := h.Spawn(context.Background(), tools.SpawnRequest{Description: "background job", Prompt: "[[bg]] write it", Mode: tools.AgentModeGeneral, Background: true}); err != nil {
		t.Fatal(err)
	}
	a, _ := h.m.Get("a1")
	waitFor(t, "the approval request", func() bool { return a.State() == subagent.WaitingApproval })
	if running, waiting, _ := h.m.Counts(); running != 1 || waiting != 1 {
		t.Fatalf("expected one agent waiting for approval, got %d/%d", running, waiting)
	}
	// /clear and the others are refused meanwhile.
	for _, command := range []func(context.Context, *config, []string) error{commandClear, commandTheme, commandLogin} {
		if err := command(context.Background(), cfg, nil); err == nil || !strings.Contains(err.Error(), "stop agents first") {
			t.Errorf("expected a refusal, got %v", err)
		}
	}
	// The wait tool serves it.
	var waited string
	captureStdout(t, func() {
		var err error
		waited, err = h.Wait(context.Background(), []string{"a1"}, true, 5*time.Second)
		if err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(waited, "bg done") {
		t.Fatalf("expected the report, got %q", waited)
	}
	if _, err := os.Stat("bg.txt"); err != nil {
		t.Error(err)
	}
}

func TestSubagentsCannotSpawnAndExploreCannotWrite(t *testing.T) {
	withToolTestColor(t, false)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[child]]": {
			newSSE("c1").text("Trying.").
				toolUse("tc1", tools.SpawnAgent, `{"description":"grandchild","prompt":"go deeper"}`).
				toolUse("tc2", "write_file", `{"path":"x.txt","content":"hi"}`).
				toolUse("tc3", "undo_last_change", `{}`).
				stop("tool_use", ""),
			newSSE("c2").text("child report: could not write").stop("end_turn", ""),
		},
	})
	_, h := agentTestConfig(t, fake, subagent.Config{}, false)
	var report string
	captureStdout(t, func() {
		var err error
		report, err = h.Spawn(context.Background(), tools.SpawnRequest{Description: "probe", Prompt: "[[child]] try things", Mode: tools.AgentModeExplore})
		if err != nil {
			t.Fatal(err)
		}
	})
	if !strings.HasPrefix(report, `<agent-report id="a1" label="probe">`) || !strings.Contains(report, "child report: could not write") {
		t.Errorf("expected the wrapped report, got %q", report)
	}

	reqs := fake.requests("[[child]]")
	if len(reqs) != 2 {
		t.Fatalf("expected two requests, got %d", len(reqs))
	}
	if system := fmt.Sprint(reqs[0].body["system"]); !strings.Contains(system, "You are a goat subagent") || strings.Contains(system, "- spawn_agent:") {
		t.Error("expected the subagent system prompt, without the subagent tools")
	}
	results := toolResultsOf(t, reqs[1])
	if len(results) != 3 {
		t.Fatalf("expected three results, got %v", results)
	}
	for i, want := range []string{tools.ErrNoAgents.Error(), "plan mode is on", "not available to subagents"} {
		if got := fmt.Sprint(results[i]["content"]); results[i]["is_error"] != true || !strings.Contains(got, want) {
			t.Errorf("result %d: expected an error with %q, got %v", i, want, got)
		}
	}
	if _, err := os.Stat("x.txt"); !os.IsNotExist(err) {
		t.Errorf("an explore agent wrote a file: %v", err)
	}
}

func TestBackgroundReportIsDeliveredOnce(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[one]]": {newSSE("o1").text("one report").stop("end_turn", "")},
		"[[two]]": {newSSE("t1").text("two report").stop("end_turn", "")},
	})
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, true)
	for _, key := range []string{"one", "two"} {
		if _, err := h.Spawn(context.Background(), tools.SpawnRequest{Description: key, Prompt: "[[" + key + "]] go", Mode: tools.AgentModeExplore, Background: true}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "both agents to finish", func() bool {
		running, _, _ := h.m.Counts()
		return running == 0
	})

	// wait takes a1's report, so it is not delivered again as a notice.
	waited, err := h.Wait(context.Background(), []string{"a1"}, true, time.Second)
	if err != nil || !strings.Contains(waited, "one report") {
		t.Fatalf("got %q %v", waited, err)
	}
	// a2's report goes with the next tool results, once, marked as automated.
	var notices string
	out := captureTerminal(t, func() { notices = h.turnNotices() })
	if strings.Contains(notices, "one report") || !strings.Contains(notices, "[automated: subagent a2 finished — not from the user]\n<agent-report id=\"a2\"") || !strings.Contains(notices, "two report") {
		t.Errorf("unexpected notices %q", notices)
	}
	if !strings.Contains(out, "a2 two finished") {
		t.Errorf("expected a line about a2, got %q", out)
	}
	if again := h.turnNotices(); again != "" {
		t.Errorf("a report was delivered twice: %q", again)
	}
	if again, _ := h.Wait(context.Background(), nil, true, time.Second); !strings.Contains(again, "No subagents are running") {
		t.Errorf("expected nothing left to wait for, got %q", again)
	}

	// Between turns, a finished agent's report goes with the next message; one an
	// earlier conversation started is only shown.
	fake.mu.Lock()
	fake.scripts["[[three]]"] = []string{newSSE("r1").text("three report").stop("end_turn", "")}
	fake.mu.Unlock()
	if _, err := h.Spawn(context.Background(), tools.SpawnRequest{Description: "three", Prompt: "[[three]] go", Mode: tools.AgentModeExplore, Background: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a3 to finish", func() bool { running, _, _ := h.m.Counts(); return running == 0 })
	activeConversationID = "c-other"
	out = captureTerminal(t, func() { collectAgentNotices(cfg) })
	if len(cfg.agentNotices) != 0 || !strings.Contains(out, "a3 three finished") {
		t.Errorf("expected another conversation's notice shown only: %q %v", out, cfg.agentNotices)
	}
}

func TestParentInterruptLeavesTheForegroundAgentIdle(t *testing.T) {
	withToolTestColor(t, false)
	fake := newRoutedAnthropic(t, map[string][]string{"[[stuck]]": {blockStream}})
	_, h := agentTestConfig(t, fake, subagent.Config{}, false)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for fake.started.Load() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	var err error
	captureStdout(t, func() {
		_, err = h.Spawn(ctx, tools.SpawnRequest{Description: "stuck", Prompt: "[[stuck]] hang", Mode: tools.AgentModeExplore})
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the spawn to end with the parent's interrupt, got %v", err)
	}
	a, _ := h.m.Get("a1")
	if a.State() != subagent.Idle || !errors.Is(a.Err(), subagent.ErrParentInterrupted) {
		t.Fatalf("expected a1 idle after the interrupt, got %s %v", a.State(), a.Err())
	}
}

func TestSpawnLimitSaysNotToRetry(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{"[[busy]]": {blockStream}})
	_, h := agentTestConfig(t, fake, subagent.Config{MaxRunning: 1}, true)
	if _, err := h.Spawn(context.Background(), tools.SpawnRequest{Description: "busy", Prompt: "[[busy]] a", Mode: tools.AgentModeExplore, Background: true}); err != nil {
		t.Fatal(err)
	}
	_, err := h.Spawn(context.Background(), tools.SpawnRequest{Description: "more", Prompt: "[[busy]] b", Mode: tools.AgentModeExplore, Background: true})
	if err == nil || !strings.Contains(err.Error(), "Subagent limit reached (1 running). Do not retry") {
		t.Fatalf("expected the limit text, got %v", err)
	}
}

func TestBackgroundIsForcedForegroundWithoutATerminal(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{"[[fg]]": {newSSE("f1").text("fg report").stop("end_turn", "")}})
	_, h := agentTestConfig(t, fake, subagent.Config{}, false)
	var report string
	captureStdout(t, func() {
		var err error
		report, err = h.Spawn(context.Background(), tools.SpawnRequest{Description: "fg", Prompt: "[[fg]] go", Mode: tools.AgentModeExplore, Background: true})
		if err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(report, "ran in the foreground") || !strings.Contains(report, "fg report") {
		t.Errorf("unexpected report %q", report)
	}
}

func TestChangesWorkspaceForSubagentTools(t *testing.T) {
	modes := map[string]string{"a1": tools.AgentModeExplore, "a2": tools.AgentModeGeneral}
	modeOf := func(id string) (string, bool) { mode, ok := modes[id]; return mode, ok }
	for _, test := range []struct {
		name, args string
		want       bool
	}{
		{tools.SpawnAgent, `{"prompt":"x"}`, false},
		{tools.SpawnAgent, `{"prompt":"x","mode":"explore"}`, false},
		{tools.SpawnAgent, `{"prompt":"x","mode":"general"}`, true},
		{tools.SendMessage, `{"agent":"a1","message":"x"}`, false},
		{tools.SendMessage, `{"agent":"a2","message":"x"}`, true},
		{tools.SendMessage, `{"agent":"a9","message":"x"}`, false},
		{tools.StopAgent, `{"agent":"a2"}`, false},
		{tools.AgentStatus, `{}`, false},
		{tools.WaitAgents, `{}`, false},
	} {
		if got := changesWorkspaceWith(provider.ToolCall{Name: test.name, Arguments: test.args}, modeOf); got != test.want {
			t.Errorf("%s %s: got %v, want %v", test.name, test.args, got, test.want)
		}
	}
}

func TestOnlyTheTopLevelPromptOffersSubagents(t *testing.T) {
	for _, codeMode := range []bool{false, true} {
		top, topSpecs := promptFor(codeMode, true)
		child, childSpecs := promptFor(codeMode, false)
		if !strings.Contains(top, "- spawn_agent:") || !strings.Contains(top, "Delegation:") || strings.Contains(top, "You are a goat subagent") {
			t.Errorf("code mode %v: the top-level prompt should list the subagent tools and delegation", codeMode)
		}
		if strings.Contains(child, "spawn_agent") || !strings.Contains(child, "You are a goat subagent") {
			t.Errorf("code mode %v: the subagent prompt should not offer subagents", codeMode)
		}
		if len(topSpecs) != len(childSpecs)+len(tools.AgentSpecs) {
			t.Errorf("code mode %v: got %d and %d tools", codeMode, len(topSpecs), len(childSpecs))
		}
		for _, spec := range childSpecs {
			if tools.PlanModePolicyFor(spec.Name) == tools.PlanModeDelegated {
				t.Errorf("a subagent is offered %s", spec.Name)
			}
		}
	}
}

func TestFinishedAgentReportGoesWithTheNextMessage(t *testing.T) {
	withToolTestColor(t, true)
	fake := newRoutedAnthropic(t, map[string][]string{
		"[[early]]": {newSSE("e1").text("early report").stop("end_turn", "")},
		"[[next]]":  {newSSE("n1").text("thanks").stop("end_turn", "")},
	})
	cfg, h := agentTestConfig(t, fake, subagent.Config{}, true)
	if _, err := h.Spawn(context.Background(), tools.SpawnRequest{Description: "early", Prompt: "[[early]] go", Mode: tools.AgentModeExplore, Background: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a1 to finish", func() bool { running, _, _ := h.m.Counts(); return running == 0 })
	out := captureTerminal(t, func() {
		if err := runPrompt(context.Background(), cfg, "[[next]] what did it find?"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "a1 early finished") {
		t.Errorf("expected a line about a1:\n%s", out)
	}
	first := fmt.Sprint(messagesOf(t, fake.requests("[[next]]")[0]))
	if !strings.Contains(first, "[automated: subagent a1 finished — not from the user]") || !strings.Contains(first, "early report") {
		t.Errorf("expected the report with the message:\n%s", first)
	}
	if len(cfg.agentNotices) != 0 {
		t.Error("the report is kept after it was sent")
	}
}
