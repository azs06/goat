package main

import (
	"errors"
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
)

func TestParseCLI(t *testing.T) {
	opts, err := parseCLI([]string{"-m", "claude-sonnet-5-5", "fix", "the", "--effort", "high", "tests", "-y"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.model != "claude-sonnet-5-5" || opts.effort != "high" || opts.prompt != "fix the tests" || !opts.autoApprove || opts.print {
		t.Fatalf("unexpected options %+v", opts)
	}

	if opts, err := parseCLI([]string{"-p", "--", "-not", "a flag"}, io.Discard); err != nil || !opts.print || opts.prompt != "-not a flag" {
		t.Fatalf("-- should end the flags: %+v %v", opts, err)
	}

	var help strings.Builder
	if _, err := parseCLI([]string{"--help"}, &help); !errors.Is(err, flag.ErrHelp) || !strings.Contains(help.String(), "--print") {
		t.Fatalf("--help: %v %q", err, help.String())
	}

	for _, bad := range [][]string{{"--effort", "huge"}, {"--model", "mystery"}, {"--bogus"}, {"-c", "-n"}} {
		if _, err := parseCLI(bad, io.Discard); err == nil {
			t.Errorf("%v: expected an error", bad)
		}
	}
}

func TestCLIModelAndEffortWin(t *testing.T) {
	setCredentials(t, true, true)
	t.Setenv("GOAT_MODEL", "gpt-6-luna")
	t.Setenv("GOAT_EFFORT", "low")
	if model, source := startupModel(nil, "claude-haiku-5-5"); model.ID != "claude-haiku-5-5" || source != "--model" {
		t.Fatalf("got %v from %s", model, source)
	}
	if effort := startupEffort(nil, "max"); effort != provider.EffortMax {
		t.Fatalf("got effort %s", effort)
	}
}

func TestOneShotLeavesTheSavedSessionAlone(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)

	fake := newFakeAnthropic(t, newSSE("msg_1").text("hello there").stop("end_turn", ""))
	saved := 0
	previous := sessionSaver
	sessionSaver = func(string, savedSession) error { saved++; return nil }
	t.Cleanup(func() { sessionSaver = previous })

	existing := provider.Conversation{Provider: provider.Anthropic, Messages: nil, ResponseID: "keep-me"}
	cfg := &config{
		model:           provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"},
		reasoningEffort: provider.EffortLow,
		conversation:    conversationState{conversation: existing},
		providers:       map[string]provider.Provider{provider.Anthropic: fake.provider(true)},
		approver:        &bashApprover{},
	}
	if code := runOneShot(cfg, cliOptions{print: true, prompt: "say hi"}); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if saved != 0 {
		t.Errorf("expected no session writes, got %d", saved)
	}
	if len(messagesOf(t, fake.request(0))) != 1 {
		t.Error("expected the one-shot prompt to start a conversation of its own")
	}

	if code := runOneShot(cfg, cliOptions{print: true, prompt: "/nosuchcommand"}); code != 1 {
		t.Errorf("expected exit code 1 when the input fails, got %d", code)
	}
}
