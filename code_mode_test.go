package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/tools"
)

// lastToolResult is the content of the last tool_result block a request sent.
func lastToolResult(t *testing.T, req recordedRequest) map[string]any {
	t.Helper()
	messages := messagesOf(t, req)
	blocks := messages[len(messages)-1].(map[string]any)["content"].([]any)
	return blocks[0].(map[string]any)
}

func toolResultText(block map[string]any) string {
	switch content := block["content"].(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, part := range content {
			parts = append(parts, fmt.Sprint(part.(map[string]any)["text"]))
		}
		return strings.Join(parts, "")
	}
	return fmt.Sprint(block["content"])
}

func toolNames(t *testing.T, req recordedRequest) []string {
	t.Helper()
	var names []string
	for _, tool := range req.body["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

func TestCodeModeOffersRunCodeOnlyWhenOn(t *testing.T) {
	chdirForTest(t, t.TempDir())
	fake := newFakeAnthropic(t, newSSE("msg_1").text("ok").stop("end_turn", ""), newSSE("msg_2").text("ok").stop("end_turn", ""))
	for _, codeMode := range []bool{false, true} {
		if _, err := sendPromptStream(context.Background(), fake.provider(true), "count TODOs", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortLow, codeMode: codeMode}); err != nil {
			t.Fatal(err)
		}
	}
	off, on := fake.request(0), fake.request(1)

	// Off, goat sends exactly what it sent before code mode existed.
	if slices.Contains(toolNames(t, off), tools.RunCode) || strings.Contains(fmt.Sprint(off.body["system"]), "run_code") {
		t.Error("with code mode off, the model must not be offered run_code")
	}
	if len(toolNames(t, off)) != len(tools.Specs) || !strings.Contains(fmt.Sprint(off.body["system"]), systemPrompt) {
		t.Error("with code mode off, the request should carry systemPrompt and tools.Specs")
	}

	names := toolNames(t, on)
	if len(names) != len(tools.Specs)+1 || names[len(names)-1] != tools.RunCode {
		t.Errorf("with code mode on, run_code should follow the other tools, got %v", names)
	}
	system := fmt.Sprint(on.body["system"])
	if !strings.Contains(system, "declare const goat: {") || !strings.Contains(system, fmt.Sprintf("about %d characters and %d lines", maxToolOutputCharsForModel, maxToolOutputLinesForModel)) {
		t.Error("the code mode prompt should describe the goat API and the result budget")
	}
	for _, spec := range tools.CodeModeSpecs {
		if !strings.Contains(codeModeSystemPrompt, "- "+spec.Name+": "+spec.Description+"\n") {
			t.Errorf("%s is missing from the code mode tool list", spec.Name)
		}
	}
	// The prompt itself is not touched either way.
	for _, req := range []recordedRequest{off, on} {
		if text := messagesOf(t, req)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string); text != "count TODOs" {
			t.Errorf("got prompt %q", text)
		}
	}
	if promptFingerprint(false) == promptFingerprint(true) {
		t.Error("the two modes should have different fingerprints")
	}
}

func TestRunCodeRoundTrip(t *testing.T) {
	chdirForTest(t, t.TempDir())
	os.WriteFile("a.txt", []byte("one\ntwo\n"), 0o644)
	os.WriteFile("b.txt", []byte("three\n"), 0o644)
	withToolTestColor(t, false)

	script := `async () => {
  const found = await goat.find_files({ path: ".", pattern: "*.txt" });
  let lines = 0;
  for (const path of found.split("\n").slice(1)) {
    lines += (await goat.read_file({ path })).split("\n").length - 2;
  }
  console.log("files read");
  return { lines };
}`
	input, _ := json.Marshal(map[string]string{"code": script})
	fake := newFakeAnthropic(t,
		newSSE("msg_1").toolUse("toolu_1", "run_code", string(input)).stop("tool_use", ""),
		newSSE("msg_2").text("3 lines.").stop("end_turn", ""),
	)

	var activity strings.Builder
	previous := activityOut
	activityOut = &activity
	t.Cleanup(func() { activityOut = previous })

	if _, err := sendPromptStream(context.Background(), fake.provider(true), "count lines", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortLow, codeMode: true}); err != nil {
		t.Fatal(err)
	}

	result := lastToolResult(t, fake.request(1))
	text := toolResultText(result)
	if result["is_error"] == true || !strings.HasPrefix(text, "run_code ran 3 goat calls.\nResult:\n{\n  \"lines\": 3\n}") || !strings.HasSuffix(text, "Console output:\nfiles read") {
		t.Fatalf("tool result %v: %q", result["is_error"], text)
	}

	shown := activity.String()
	for _, want := range []string{
		"* Code(const found = await goat.find_files({ path: \".\", pattern: \"*.txt\" });) [9 lines]",
		"  * Find(*.txt in .)",
		"  * Read(a.txt)",
		"  * Read(b.txt)",
		"Ran 3 calls",
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("activity is missing %q:\n%s", want, shown)
		}
	}
	if strings.Contains(shown, "\x1b") {
		t.Error("plain output must not contain escape codes")
	}

	entries, _ := loadConversationHistoryEntries()
	var recorded []string
	for _, entry := range entries {
		if entry.Type == "tool_call" || entry.Type == "tool_output" {
			recorded = append(recorded, entry.Type+":"+entry.ToolName)
		}
	}
	if strings.Join(recorded, ",") != "tool_call:run_code,tool_output:run_code" {
		t.Errorf("history should hold the script and its result only, got %v", recorded)
	}
}

func TestRunCodeIsRefusedWhenCodeModeIsOff(t *testing.T) {
	chdirForTest(t, t.TempDir())
	withToolTestColor(t, false)
	fake := newFakeAnthropic(t,
		newSSE("msg_1").toolUse("toolu_1", "run_code", `{"code":"async () => 1"}`).stop("tool_use", ""),
		newSSE("msg_2").text("ok").stop("end_turn", ""),
	)
	previous := activityOut
	activityOut = io.Discard
	t.Cleanup(func() { activityOut = previous })

	if _, err := sendPromptStream(context.Background(), fake.provider(true), "go", conversationState{}, turnOptions{model: "claude-opus-5-5", effort: provider.EffortLow}); err != nil {
		t.Fatal(err)
	}
	result := lastToolResult(t, fake.request(1))
	if result["is_error"] != true || !strings.Contains(toolResultText(result), "code mode is off") {
		t.Fatalf("got %v", result)
	}
}

func TestNestedRendererCapsTheCallsShown(t *testing.T) {
	withToolTestColor(t, false)
	var out strings.Builder
	previous := activityOut
	activityOut = &out
	t.Cleanup(func() { activityOut = previous })

	r := &nestedRenderer{}
	for i := 0; i < maxNestedCallsShown+5; i++ {
		done := r.Start(provider.ToolCall{ID: fmt.Sprint(i), Name: "read_file", Arguments: fmt.Sprintf(`{"path":"f%d.txt"}`, i)})
		done("File contents for f.txt:\nx", nil)
	}
	r.finish()
	text := out.String()
	if strings.Count(text, "* Read(") != maxNestedCallsShown || !strings.HasSuffix(text, "... +5 calls not shown\n") {
		t.Fatalf("got:\n%s", text)
	}
	if !strings.Contains(text, "  * Read(f0.txt)\n    | Read 1 line\n") {
		t.Errorf("nested calls should be indented under the script:\n%s", text)
	}
}

func TestCommandCodeMode(t *testing.T) {
	withColor(t, false)
	cfg := &config{}
	for _, step := range []struct {
		args []string
		want bool
	}{{nil, true}, {nil, false}, {[]string{"on"}, true}, {[]string{"on"}, true}, {[]string{"off"}, false}} {
		captureStdout(t, func() {
			if err := commandCodeMode(context.Background(), cfg, step.args); err != nil {
				t.Fatal(err)
			}
		})
		if cfg.codeMode != step.want {
			t.Fatalf("/codemode %v: code mode %v, want %v", step.args, cfg.codeMode, step.want)
		}
	}
	if err := commandCodeMode(context.Background(), cfg, []string{"maybe"}); err == nil {
		t.Error("expected an error for an unknown argument")
	}

	cfg.codeMode = true
	if !strings.Contains(strings.Join(footerParts(cfg), " | "), "code mode") {
		t.Error("the footer should show code mode")
	}
}

func TestCodeModeFlagAndEnv(t *testing.T) {
	opts, err := parseCLI([]string{"--codemode", "go"}, io.Discard)
	if err != nil || !opts.codeMode || opts.prompt != "go" {
		t.Fatalf("got %+v %v", opts, err)
	}

	chdirForTest(t, t.TempDir())
	t.Setenv("GOAT_CODEMODE", "1")
	if cfg := loadConfig(cliOptions{}); !cfg.codeMode {
		t.Error("GOAT_CODEMODE=1 should start in code mode")
	}
	t.Setenv("GOAT_CODEMODE", "")
	if cfg := loadConfig(cliOptions{codeMode: true}); !cfg.codeMode {
		t.Error("--codemode should start in code mode")
	}
	if cfg := loadConfig(cliOptions{}); cfg.codeMode {
		t.Error("code mode should be off by default")
	}
}

func TestToolListShowsRunCode(t *testing.T) {
	withColor(t, false)
	if list := renderToolList(); !strings.Contains(list, "Code mode") || !strings.Contains(list, "run_code (code)") {
		t.Errorf("/tools should list run_code under Code mode:\n%s", list)
	}
}
