package main

import (
	"strings"
	"testing"
	"time"

	"github.com/azs06/goat/internal/provider"

	"github.com/azs06/goat/internal/style"
)

// withColor turns styling on or off for the rest of the test.
func withColor(t *testing.T, enabled bool) {
	t.Helper()
	t.Cleanup(style.SetEnabled(enabled))
	t.Cleanup(style.SetTrueColor(false))
}

func chromeAssertPlain(t *testing.T, name, text string) {
	t.Helper()
	if strings.ContainsAny(text, "\x1b\r") {
		t.Errorf("%s: plain output contains escapes or carriage returns: %q", name, text)
	}
}

func TestShortenHome(t *testing.T) {
	cases := []struct{ path, home, want string }{
		{"/Users/me/code/goat", "/Users/me", "~/code/goat"},
		{"/Users/me", "/Users/me", "~"},
		{"/Users/meow/x", "/Users/me", "/Users/meow/x"},
		{"/tmp", "", "/tmp"},
	}
	for _, tc := range cases {
		if got := shortenHome(tc.path, tc.home); got != tc.want {
			t.Errorf("shortenHome(%q, %q) = %q, want %q", tc.path, tc.home, got, tc.want)
		}
	}
}

func TestTruncateToWidth(t *testing.T) {
	withColor(t, true)
	if got := style.TruncateToWidth("hello", 10); got != "hello" {
		t.Errorf("short text changed: %q", got)
	}
	if got := style.TruncateToWidth("hello world", 6); got != "hello…" {
		t.Errorf("got %q", got)
	}
	if got := style.TruncateToWidth("hello", 0); got != "" {
		t.Errorf("zero width: %q", got)
	}
}

func TestRenderBanner(t *testing.T) {
	withColor(t, false)
	plain := renderBanner("openai:gpt-6", "medium", "~/code", "", 80)
	chromeAssertPlain(t, "banner", plain)
	if !strings.Contains(plain, "Welcome to G.O.A.T agent") || !strings.Contains(plain, "Model: openai:gpt-6 (effort medium)") {
		t.Errorf("plain banner lost its text: %q", plain)
	}

	withColor(t, true)
	for _, width := range []int{20, 50, 200} {
		banner := renderBanner("anthropic:claude-opus-5-5", "high", "~/a/very/long/path/that/does/not/fit/in/a/narrow/terminal", "", width)
		lines := strings.Split(banner, "\n")
		boxWidth := style.VisibleWidth(lines[0])
		if boxWidth > maxBannerWidth || boxWidth > width && width >= 20 {
			t.Errorf("width %d: box is %d wide", width, boxWidth)
		}
		for _, line := range lines {
			if style.VisibleWidth(line) != boxWidth {
				t.Errorf("width %d: ragged line %q", width, style.StripANSI(line))
			}
		}
		if !strings.Contains(style.StripANSI(banner), "✻ Welcome") {
			t.Errorf("width %d: banner lost its title", width)
		}
	}
}

func TestRenderFooterFitsWidth(t *testing.T) {
	withColor(t, true)
	footer := renderFooter([]string{"anthropic:claude-opus-5-5", "effort high", "~12k ctx", "auto-approve on", "/help for commands"}, 40)
	if w := style.VisibleWidth(footer); w > 39 {
		t.Errorf("footer is %d wide, want at most 39", w)
	}
	if !strings.Contains(style.StripANSI(renderFooter([]string{"m", "effort low"}, 80)), "m · effort low") {
		t.Errorf("footer parts not joined")
	}
}

func TestFormatTokenCount(t *testing.T) {
	for tokens, want := range map[int64]string{0: "0", 850: "850", 1000: "1k", 12345: "12k", 150500: "151k"} {
		if got := formatTokenCount(tokens); got != want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", tokens, got, want)
		}
	}
}

func TestInputRowsAndCleanup(t *testing.T) {
	cases := []struct {
		text  string
		width int
		want  int
	}{
		{"", 80, 1},
		{"hello", 80, 1},
		{strings.Repeat("x", 78), 80, 1},
		{strings.Repeat("x", 79), 80, 2},
		{strings.Repeat("x", 200), 80, 3},
	}
	for _, tc := range cases {
		if got := inputRows(2, tc.text, tc.width); got != tc.want {
			t.Errorf("inputRows(%d chars, %d) = %d, want %d", len(tc.text), tc.width, got, tc.want)
		}
	}

	withColor(t, true)
	// One input row plus the rule above it.
	if got := renderPromptCleanup("hi", 80); got != "\x1b[2A\r\x1b[J" {
		t.Errorf("cleanup = %q", got)
	}
}

func TestRenderPromptFrameEndsOnPromptLine(t *testing.T) {
	withColor(t, true)
	frame := renderPromptFrame(renderFooter([]string{"model"}, 40), 40)
	if !strings.HasSuffix(frame, "\x1b[2A\x1b[3G") {
		t.Errorf("frame does not move back to the prompt: %q", frame)
	}
	if strings.Count(frame, "\n") != 3 {
		t.Errorf("frame should be four lines: %q", frame)
	}
}

func TestRenderUserMessage(t *testing.T) {
	withColor(t, false)
	chromeAssertPlain(t, "user message", renderUserMessage("fix it", 80))
	withColor(t, true)
	if got := style.StripANSI(renderUserMessage("fix it", 80)); got != "❯ fix it " {
		t.Errorf("got %q", got)
	}
}

func TestRenderWorked(t *testing.T) {
	withColor(t, true)
	if got := style.StripANSI(renderWorked(12*time.Second + 400*time.Millisecond)); got != "✻ Worked for 12s" {
		t.Errorf("got %q", got)
	}
}

func TestRenderApprovalCard(t *testing.T) {
	withColor(t, true)
	raw := renderApprovalCard("echo hi\nrm -rf \x1b[2Jbuild", "sub dir", 80)
	if strings.Contains(raw, "\x1b[2J") {
		t.Errorf("command escape reached the terminal: %q", raw)
	}
	card := style.StripANSI(raw)
	for _, want := range []string{"╭─ Run command?", "in sub dir", "│ echo hi", "│ rm -rf \\x1b[2Jbuild", "[y]es · [n]o · [a]lways", "❯ "} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q:\n%s", want, card)
		}
	}
}

func TestRenderHelpTable(t *testing.T) {
	withColor(t, false)
	plain := renderHelpTable(commands)
	chromeAssertPlain(t, "help", plain)
	for _, want := range []string{"Conversation\n  /clear ", "Start a new conversation", "(also /new, /reset)", "Workspace\n  /plan [on|off]"} {
		if !strings.Contains(plain, want) {
			t.Errorf("plain help lacks %q:\n%s", want, plain)
		}
	}

	// Colored help wraps long descriptions under their column, but says the same.
	withColor(t, true)
	colored := style.StripANSI(renderHelpTable(commands))
	if strings.Join(strings.Fields(colored), " ") != strings.Join(strings.Fields(plain), " ") {
		t.Errorf("colored help differs from plain once unstyled:\n%s", colored)
	}
	for _, line := range strings.Split(colored, "\n") {
		if style.VisibleWidth(line) >= terminalWidth() {
			t.Errorf("line too wide: %q", line)
		}
	}
}

func TestRenderKeyValues(t *testing.T) {
	withColor(t, false)
	got := renderKeyValues([][2]string{{"Model:", "x"}, {"Workspace:", "/w"}})
	if got != "Model:     x\nWorkspace: /w" {
		t.Errorf("got %q", got)
	}
}

func TestRenderModelChoices(t *testing.T) {
	a := provider.Model{Provider: provider.OpenAI, ID: "gpt-a"}
	b := provider.Model{Provider: provider.Anthropic, ID: "claude-b"}

	withColor(t, false)
	got := renderModelChoices([]provider.Model{a, b}, b)
	want := "   1. " + a.String() + "\n*  2. " + b.String()
	if got != want {
		t.Errorf("plain listing = %q, want %q", got, want)
	}

	withColor(t, true)
	if colored := style.StripANSI(renderModelChoices([]provider.Model{a, b}, b)); !strings.Contains(colored, "❯  2. "+b.String()) {
		t.Errorf("colored listing = %q", colored)
	}
}

func TestRenderUserMessageEscapesAndFitsWidth(t *testing.T) {
	withColor(t, true)
	if raw := renderUserMessage("up\x1b[Aarrow", 80); strings.Contains(style.StripANSI(raw), "\x1b") {
		t.Errorf("control characters reached the terminal: %q", raw)
	}
	// "❯ " plus 48 cells fills a 50-column row exactly; the pad would wrap.
	if got := style.StripANSI(renderUserMessage(strings.Repeat("A", 48), 50)); style.VisibleWidth(got) != 50 {
		t.Errorf("pad space should be dropped at the edge: %d cells", style.VisibleWidth(got))
	}
	if got := inputRows(2, "ab\x1b[A", 6); got != 2 {
		t.Errorf("ESC echoes as two cells: got %d rows", got)
	}
}

func TestRenderApprovalCardWrapsLongCommands(t *testing.T) {
	withColor(t, true)
	card := style.StripANSI(renderApprovalCard("echo "+strings.Repeat("x", 120), ".", 50))
	lines := strings.Split(card, "\n")
	joined := ""
	for _, line := range lines[2 : len(lines)-1] {
		if !strings.HasPrefix(line, "│ ") || style.VisibleWidth(line) > 49 {
			t.Errorf("row not framed or too wide: %q", line)
		}
		joined += strings.TrimPrefix(line, "│ ")
	}
	if joined != "echo "+strings.Repeat("x", 120) {
		t.Errorf("command not shown in full: %q", joined)
	}
}
