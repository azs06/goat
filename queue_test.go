package main

import (
	"context"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
)

func resetTurnQueue(t *testing.T) {
	t.Helper()
	reset := func() {
		turnQueue.mu.Lock()
		turnQueue.items = nil
		turnQueue.editor = promptEditor{}
		turnQueue.capturing = false
		turnQueue.footer = nil
		turnQueue.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func typeQueueKeys(text string) {
	for _, r := range text {
		queueKey(string(r))
	}
}

func TestQueueKeysQueueLinesAndUpTakesTheLastBack(t *testing.T) {
	resetTurnQueue(t)

	typeQueueKeys("fix the tests\r")
	typeQueueKeys("  \r") // blank lines are not queued
	typeQueueKeys("/compact\r")
	typeQueueKeys("then the readme\r")
	if got := strings.Join(turnQueue.items, "|"); got != "fix the tests|/compact|then the readme" {
		t.Fatalf("unexpected queue %q", got)
	}

	queueKey("\x1b[A")
	if string(turnQueue.editor.buf) != "then the readme" || len(turnQueue.items) != 2 {
		t.Fatalf("expected Up to take the last message back, got %q and %v", string(turnQueue.editor.buf), turnQueue.items)
	}
	queueKey("\x1b[A")
	if len(turnQueue.items) != 2 {
		t.Fatal("expected Up to do nothing while a line is being typed")
	}
	queueKey("\x04")
	queueKey("\t")
	if string(turnQueue.editor.buf) != "then the readme" {
		t.Fatalf("expected Ctrl-D and Tab to be ignored, got %q", string(turnQueue.editor.buf))
	}
}

func TestQueuedInputOrder(t *testing.T) {
	resetTurnQueue(t)
	turnQueue.items = []string{"one", "two", "/compact", "three"}

	if text := takeQueuedText(); text != "one\ntwo" {
		t.Fatalf("expected the leading text joined, got %q", text)
	}
	if text := takeQueuedText(); text != "" {
		t.Fatalf("expected steering to stop at a command, got %q", text)
	}
	if text, ok := nextQueuedInput(); !ok || text != "/compact" {
		t.Fatalf("expected the command on its own, got %q %v", text, ok)
	}
	if text, ok := nextQueuedInput(); !ok || text != "three" {
		t.Fatalf("expected the remaining text, got %q %v", text, ok)
	}
	if _, ok := nextQueuedInput(); ok {
		t.Fatal("expected an empty queue")
	}

	// A ! shell command waits for the turn to end and runs on its own, like a slash
	// command.
	turnQueue.items = []string{"look", "!git status", "then this"}
	if text := takeQueuedText(); text != "look" {
		t.Fatalf("expected steering to stop at the shell command, got %q", text)
	}
	if text, ok := nextQueuedInput(); !ok || text != "!git status" {
		t.Fatalf("expected the shell command on its own, got %q %v", text, ok)
	}
}

func TestInterruptReturnsTheQueueToThePrompt(t *testing.T) {
	resetTurnQueue(t)
	turnQueue.items = []string{"one", "/compact"}
	input := newLineReader(strings.NewReader(""))
	input.partial = "half typed"

	returnQueueToPrompt(input)
	if input.partial != "one /compact half typed" || len(turnQueue.items) != 0 {
		t.Fatalf("expected everything back in the prompt, got %q and %v", input.partial, turnQueue.items)
	}
}

func TestKeyCaptureCarriesTheUnsentLine(t *testing.T) {
	resetTurnQueue(t)
	input := newLineReader(strings.NewReader("queued\runsent"))
	input.partial = "from before: "

	startKeyCapture(context.Background(), input)
	// The reader runs out of input after "unsent"; wait for it to stop.
	<-activeKeyCapture.done
	stopKeyCapture()

	if text := takeQueuedText(); text != "from before: queued" {
		t.Fatalf("expected the earlier line to be continued and queued, got %q", text)
	}
	if input.partial != "unsent" {
		t.Fatalf("expected the unsent line back in the prompt, got %q", input.partial)
	}
}

func TestQueuedInputLines(t *testing.T) {
	resetTurnQueue(t)
	withColor(t, true)

	if lines, _ := queuedInputLines(80); lines != nil {
		t.Fatalf("expected nothing while keys are not captured, got %q", lines)
	}

	turnQueue.capturing = true
	lines, cursor := queuedInputLines(80)
	if len(lines) != 1 || !strings.Contains(style.StripANSI(lines[0]), "type a message") || cursor != style.VisibleWidth(promptPrefix())+1 {
		t.Fatalf("expected the empty input line with a hint, got %q (cursor %d)", lines, cursor)
	}

	turnQueue.items = []string{"first"}
	turnQueue.editor.setText(strings.Repeat("x", 200))
	lines, cursor = queuedInputLines(40)
	if len(lines) != 2 || !strings.Contains(style.StripANSI(lines[0]), "queued") {
		t.Fatalf("expected the queued message above the input, got %q", lines)
	}
	for _, line := range lines {
		if width := style.VisibleWidth(line); width >= 40 {
			t.Errorf("expected every line to fit, got width %d: %q", width, line)
		}
	}
	if cursor > 40 {
		t.Errorf("expected the cursor inside the terminal, got column %d", cursor)
	}
}

func TestQueuedTextSteersTheNextToolRound(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t,
		newSSE("msg_1").toolUse("toolu_1", "read_dir", `{"path":"."}`).stop("tool_use", ""),
		newSSE("msg_2").text("Done.").stop("end_turn", ""),
	)
	steering := []string{"also check the tests"}
	takeSteering := func() string {
		if len(steering) == 0 {
			return ""
		}
		text := steering[0]
		steering = steering[1:]
		return text
	}

	if _, err := sendPromptStream(context.Background(), fake.provider(true), "list files", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortMedium, takeSteering: takeSteering}); err != nil {
		t.Fatalf("sendPromptStream: %v", err)
	}

	second := messagesOf(t, fake.request(1))
	if got := strings.Join(contentTypes(second[2]), ","); got != "tool_result,text" {
		t.Fatalf("expected the tool result followed by the queued text, got %s", got)
	}
	if text := second[2].(map[string]any)["content"].([]any)[1].(map[string]any)["text"]; text != "also check the tests" {
		t.Fatalf("expected the queued text, got %v", text)
	}

	entries, _ := loadConversationHistoryEntries()
	found := false
	for _, entry := range entries {
		found = found || entry.Type == "user" && entry.Content == "also check the tests"
	}
	if !found {
		t.Error("expected the queued message in the history")
	}
}

func TestQueuedPasteStaysOneMessage(t *testing.T) {
	resetTurnQueue(t)
	queueKey(pasteStart)
	typeQueueKeys("line one\rline two")
	queueKey(pasteEnd)
	typeQueueKeys("\r")
	if len(turnQueue.items) != 1 || turnQueue.items[0] != "line one\nline two" {
		t.Fatalf("expected the paste queued as one message, got %q", turnQueue.items)
	}
}

func TestTurnFrameLinesFrameTheInputLikeThePrompt(t *testing.T) {
	resetTurnQueue(t)
	withColor(t, true)
	if lines, _, _ := turnFrameLines(80, 20); lines != nil {
		t.Fatalf("expected nothing while keys are not captured, got %q", lines)
	}
	turnQueue.capturing = true

	// Without a footer the input is drawn on its own.
	lines, row, _ := turnFrameLines(80, 20)
	if len(lines) != 1 || row != 0 {
		t.Fatalf("got %q, row %d", lines, row)
	}

	setTurnFooter([]string{"anthropic:claude-opus-5-5", "effort high"})
	turnQueue.items = []string{"queued one"}
	turnQueue.editor.setText("typing")
	for _, width := range []int{20, 40, 120} {
		lines, row, cursor := turnFrameLines(width, 20)
		plain := make([]string, len(lines))
		for i, line := range lines {
			plain[i] = style.StripANSI(line)
			if w := style.VisibleWidth(line); w >= width {
				t.Errorf("width %d: line %d is %d wide", width, i, w)
			}
		}
		if len(lines) != 5 || row != 2 || !strings.Contains(plain[0], "queued") || !strings.HasPrefix(plain[1], style.Glyphs().HLine) ||
			!strings.Contains(plain[2], "typing") || !strings.HasPrefix(plain[3], style.Glyphs().HLine) || !strings.Contains(plain[4], "anthropic") {
			t.Fatalf("width %d: got %q, row %d", width, plain, row)
		}
		if cursor != style.VisibleWidth(promptPrefix())+len("typing")+1 {
			t.Errorf("width %d: cursor %d", width, cursor)
		}
	}

	// Too short a screen for the frame: the input on its own again.
	if lines, row, _ := turnFrameLines(80, 4); len(lines) != 2 || row != 1 {
		t.Fatalf("got %q, row %d", lines, row)
	}
}
