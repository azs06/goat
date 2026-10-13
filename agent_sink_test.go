package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azs06/goat/internal/provider"
)

// recordingSink keeps what a turn shows, the way a subagent's sink does.
type recordingSink struct {
	mu     sync.Mutex
	events []string
}

func (s *recordingSink) add(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *recordingSink) Text(text string) { s.add("text:" + text) }
func (s *recordingSink) EndText()         { s.add("end") }
func (s *recordingSink) ToolStart(c provider.ToolCall) {
	s.add("start:" + c.Name)
}
func (s *recordingSink) ToolResult(c provider.ToolCall, _ string, err error, _ time.Duration) {
	s.add(fmt.Sprintf("result:%s:%v", c.Name, err == nil))
}
func (s *recordingSink) User(text string)    { s.add("user:" + text) }
func (s *recordingSink) Notice(text string)  { s.add("notice:" + text) }
func (s *recordingSink) Status(label string) { s.add("status:" + label) }

// captureTerminal returns every byte fn writes to stdout, stderr or activityOut.
func captureTerminal(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, activity := os.Stdout, os.Stderr, activityOut
	os.Stdout, os.Stderr, activityOut = writer, writer, writer
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(reader)
		done <- string(data)
	}()
	defer func() { os.Stdout, os.Stderr, activityOut = stdout, stderr, activity }()
	fn()
	os.Stdout, os.Stderr, activityOut = stdout, stderr, activity
	writer.Close()
	return <-done
}

func TestSinkTurnWritesNothingToTheTerminal(t *testing.T) {
	chdirForTest(t, t.TempDir())
	// Color on, so a stray spinner or markdown render would write.
	withToolTestColor(t, true)
	previous := activeConversationID
	t.Cleanup(func() { activeConversationID = previous })
	activeConversationID = "c-main"

	var long []string
	for i := 1; i <= 1000; i++ {
		long = append(long, fmt.Sprint("line ", i))
	}
	os.WriteFile("long.txt", []byte(strings.Join(long, "\n")+"\n"), 0o644)
	script, _ := json.Marshal(map[string]string{"code": `async () => (await goat.read_file({ path: "long.txt", limit: 2 })).length`})
	fake := newFakeAnthropic(t,
		newSSE("msg_1").text("Let me **look**.\n").toolUse("toolu_1", "run_code", string(script)).stop("tool_use", ""),
		newSSE("msg_2").text("Reading it all.").toolUse("toolu_2", "read_file", `{"path":"long.txt"}`).stop("tool_use", ""),
		newSSE("msg_3").text("Done.").stop("end_turn", ""),
	)
	sink := &recordingSink{}
	var waited atomic.Int64
	steered := false
	opts := turnOptions{
		model: "claude-opus-5-5", effort: provider.EffortLow, codeMode: true,
		conversationID: "c-child", parentID: "c-main", sink: sink, inputWait: &waited,
		takeSteering: func() string {
			if steered {
				return ""
			}
			steered = true
			return "also check b.txt"
		},
	}
	out := captureTerminal(t, func() {
		if _, err := sendPromptStream(context.Background(), fake.provider(true), "look around", conversationState{}, opts); err != nil {
			t.Fatal(err)
		}
	})
	if out != "" {
		t.Fatalf("a turn with a sink wrote to the terminal: %q", out)
	}

	events := strings.Join(sink.events, "\n")
	for _, want := range []string{"status:Thinking", "text:Let me **look**.\n", "start:run_code", "start:read_file", "result:read_file:true", "result:run_code:true", "user:also check b.txt", "text:Done.", "end"} {
		if !slices.Contains(sink.events, want) {
			t.Errorf("expected %q among the sink's events:\n%s", want, events)
		}
	}
	// The script's own call shows between the script's start and result.
	if i, j, k := slices.Index(sink.events, "start:run_code"), slices.Index(sink.events, "start:read_file"), slices.Index(sink.events, "result:run_code:true"); i >= j || j >= k {
		t.Errorf("expected the nested call inside the script's events:\n%s", events)
	}

	entries, err := loadConversationHistoryEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected history entries")
	}
	for _, entry := range entries {
		if entry.ConversationID != "c-child" || entry.ParentID != "c-main" {
			t.Errorf("expected the child's conversation and parent on every entry, got %+v", entry)
		}
	}
	// The long read was shortened for the model, and its note numbers the output.
	result := toolResultText(lastToolResult(t, fake.request(2)))
	if !strings.Contains(result, "read_output with output 2 shows all of it") {
		t.Errorf("expected the shortened note for output 2, got %q", result[max(0, len(result)-200):])
	}
}

func TestTerminalSinkEndsPlainTextWithANewline(t *testing.T) {
	withToolTestColor(t, false)
	out := captureStdout(t, func() {
		sink := &terminalSink{}
		sink.Status("Thinking")
		sink.EndText()
		sink.Text("no newline")
		sink.EndText()
	})
	if out != "no newline\n" {
		t.Fatalf("got %q", out)
	}
}

func TestRecordToolOutputNumberedIsUniqueUnderConcurrency(t *testing.T) {
	chdirForTest(t, t.TempDir())
	const goroutines, each = 8, 10
	numbers := make(chan int, goroutines*each)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opts := turnOptions{conversationID: fmt.Sprint("c-", g), parentID: "c-main"}
			for i := range each {
				response := fmt.Sprintf("r%d-%d", g, i)
				opts.record(historyEntry{Type: "tool_call", ToolName: "read_file", ResponseID: response})
				n, ok := recordToolOutputNumbered(opts.stamp(historyEntry{Type: "tool_output", ToolName: "read_file", Content: response, ResponseID: response, Status: "ok"}))
				if !ok {
					t.Error("expected a number")
					return
				}
				// The number names this goroutine's output.
				if _, content, err := toolOutputByNumber(n); err != nil || content != response {
					t.Errorf("output %d: got %q %v, want %q", n, content, err, response)
				}
				numbers <- n
			}
		}()
	}
	wg.Wait()
	close(numbers)
	var got []int
	for n := range numbers {
		got = append(got, n)
	}
	slices.Sort(got)
	for i, n := range got {
		if n != i+1 {
			t.Fatalf("expected the numbers 1 to %d once each, got %v", goroutines*each, got)
		}
	}
	if len(got) != goroutines*each {
		t.Fatalf("got %d numbers", len(got))
	}
}

func TestInputWaitCountsAgainstTheTurn(t *testing.T) {
	var waited atomic.Int64
	addInputWait(turnOptions{inputWait: &waited}, time.Second)
	if got := inputWaited(&waited); got != time.Second {
		t.Fatalf("got %s", got)
	}
	before := inputWaited(nil)
	t.Cleanup(func() { addInputWait(turnOptions{}, -time.Second) })
	addInputWait(turnOptions{}, time.Second)
	if got := inputWaited(nil) - before; got != time.Second {
		t.Fatalf("the global counter grew by %s", got)
	}
}
