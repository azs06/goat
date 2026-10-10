package codemode

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// echoHost answers every call with the call's name and arguments, and fails "fail".
func echoHost(calls *[]string) Host {
	return Host{
		Names: []string{"read_file", "fail", "slow"},
		Call: func(ctx context.Context, name, args string) (string, error) {
			if calls != nil {
				*calls = append(*calls, name+" "+args)
			}
			switch name {
			case "fail":
				return "", errors.New("goat.fail: it failed")
			case "slow":
				select {
				case <-ctx.Done():
					return "", fmt.Errorf("goat.slow: %w", ctx.Err())
				case <-time.After(5 * time.Second):
					return "slept", nil
				}
			}
			return "contents of " + args, nil
		},
	}
}

func runCode(t *testing.T, code string) (Result, error) {
	t.Helper()
	return Run(context.Background(), code, echoHost(nil), DefaultLimits)
}

func TestCodeShapes(t *testing.T) {
	for _, tc := range []struct{ name, code, want string }{
		{"arrow", `async () => { const a = await goat.read_file({ path: "a" }); return a; }`, `contents of {"path":"a"}`},
		{"arrow with trailing comment", "async () => { return 1 } // done", "1"},
		{"named function", "async function main() { return \"x\" }", "x"},
		{"function body", "const r = await goat.read_file({path: \"b\"});\nreturn r.length;", "24"},
		{"bare call", `goat.read_file({ path: "c" })`, `contents of {"path":"c"}`},
		{"fenced js", "```js\nasync () => 'fenced'\n```", "fenced"},
		{"fenced typescript tag", "```typescript\nreturn 'ts tag'\n```", "ts tag"},
		{"export default", "export default async () => 'exported';", "exported"},
		{"iife", "(async () => 'iife')()", "iife"},
		{"then chain", `goat.read_file({ path: "d" }).then(t => t.toUpperCase())`, `CONTENTS OF {"PATH":"D"}`},
		{"promise all", `async () => (await Promise.all(["x", "y"].map(p => goat.read_file({ path: p })))).length`, "2"},
		{"modern syntax", "async () => { const { a, b = 2 } = { a: 1 }; const s = `${a}-${b}`; return s + (null ?? '!') + ({x:{y:3}}?.x?.y); }", "1-2!3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := runCode(t, tc.code)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if result.Value != tc.want {
				t.Errorf("value %q, want %q", result.Value, tc.want)
			}
		})
	}
}

func TestResultFormatting(t *testing.T) {
	result, err := runCode(t, `async () => ({ files: ["a", "b"], count: 2 })`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Value != "{\n  \"files\": [\n    \"a\",\n    \"b\"\n  ],\n  \"count\": 2\n}" {
		t.Errorf("object: %q", result.Value)
	}

	result, err = runCode(t, `async () => Array.from({ length: 500 }, (_, i) => i)`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Value, "\n") || !strings.HasPrefix(result.Value, "[0,1,2") {
		t.Errorf("a long result should be compact: %.40q", result.Value)
	}

	result, err = runCode(t, `async () => { goat.read_file({ path: "x" }); }`)
	if err != nil || result.HasValue || result.Calls != 1 {
		t.Errorf("no return value: %+v %v", result, err)
	}

	result, err = runCode(t, `async () => { const a = {}; a.self = a; return a; }`)
	if err != nil || result.Value != "[object Object]" {
		t.Errorf("cyclic: %q %v", result.Value, err)
	}

	result, err = runCode(t, `async () => 10n`)
	if err != nil || result.Value != "10" {
		t.Errorf("bigint: %q %v", result.Value, err)
	}
}

func TestConsoleIsCaptured(t *testing.T) {
	result, err := runCode(t, `async () => { console.log("a", 1, { b: 2 }); console.warn("careful"); console.error("bad"); return "done" }`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Logs != "a 1 {\"b\":2}\n[warn] careful\n[error] bad" {
		t.Errorf("logs %q", result.Logs)
	}

	result, err = runCode(t, `async () => { for (let i = 0; i < 100000; i++) console.log("line " + i); }`)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Logs) > DefaultLimits.MaxLogBytes+100 || !strings.HasSuffix(result.Logs, "[... further console output dropped ...]") {
		t.Errorf("logs not capped: %d bytes, ends %q", len(result.Logs), result.Logs[len(result.Logs)-40:])
	}

	_, err = runCode(t, `async () => { console.log("before"); throw new Error("boom") }`)
	var runErr *Error
	if !errors.As(err, &runErr) || runErr.Logs != "before" {
		t.Errorf("logs should survive a failure: %v", err)
	}
}

func TestHostErrorsAreCatchable(t *testing.T) {
	result, err := runCode(t, `async () => { try { await goat.fail({}) } catch (e) { return "caught: " + e.message } }`)
	if err != nil || result.Value != "caught: goat.fail: it failed" {
		t.Fatalf("got %q %v", result.Value, err)
	}
	if result.Calls != 1 || result.Failed != 1 {
		t.Errorf("counts %d/%d", result.Calls, result.Failed)
	}
}

func TestUncaughtErrorReportsThePosition(t *testing.T) {
	_, err := runCode(t, "async () => {\n  const a = 1;\n  await goat.fail({});\n}")
	if err == nil || !strings.Contains(err.Error(), "line 3") || !strings.Contains(err.Error(), "goat.fail: it failed") {
		t.Fatalf("got %v", err)
	}
	_, err = runCode(t, "const x = 1;\nnull.y;")
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("body form: %v", err)
	}
}

func TestSyntaxErrors(t *testing.T) {
	_, err := runCode(t, "async () => {\n  const a: number = 1;\n}")
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "TypeScript") {
		t.Fatalf("got %v", err)
	}
	_, err = Run(context.Background(), strings.Repeat("x", DefaultLimits.MaxCodeBytes+1), echoHost(nil), DefaultLimits)
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("oversized code: %v", err)
	}
}

func TestArgumentChecks(t *testing.T) {
	var calls []string
	result, err := Run(context.Background(), `async () => {
		const errors = [];
		for (const bad of ["a.txt", ["a"], () => 1]) {
			try { await goat.read_file(bad) } catch (e) { errors.push(e.message) }
		}
		await goat.read_file();
		return errors;
	}`, echoHost(&calls), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(result.Value, "takes one object argument") != 3 {
		t.Errorf("value %s", result.Value)
	}
	if len(calls) != 1 || calls[0] != "read_file {}" {
		t.Errorf("calls %v", calls)
	}
}

func TestCallLimit(t *testing.T) {
	limits := DefaultLimits
	limits.MaxCalls = 3
	var calls []string
	result, err := Run(context.Background(), `async () => {
		let errors = 0;
		for (let i = 0; i < 6; i++) { try { await goat.read_file({ path: String(i) }) } catch (e) { errors++ } }
		return errors;
	}`, echoHost(&calls), limits)
	if err != nil {
		t.Fatal(err)
	}
	if result.Value != "3" || len(calls) != 3 {
		t.Errorf("value %s, %d calls ran", result.Value, len(calls))
	}
}

func TestTimeoutStopsLoops(t *testing.T) {
	limits := DefaultLimits
	limits.Timeout = 200 * time.Millisecond
	for _, code := range []string{
		"while (true) {}",
		"async () => { await 1; while (true) {} }",
		"async () => { for (;;) { try { while (true) {} } catch (e) {} } }",
	} {
		start := time.Now()
		_, err := Run(context.Background(), code, echoHost(nil), limits)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Errorf("%s: got %v", code, err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("%s: took %v", code, elapsed)
		}
	}
}

func TestTimeoutReachesHostCalls(t *testing.T) {
	limits := DefaultLimits
	limits.Timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := Run(context.Background(), `async () => { try { await goat.slow({}) } catch (e) {} return "kept going" }`, echoHost(nil), limits)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the host call was not cancelled")
	}
}

func TestPausedClockDoesNotTimeOut(t *testing.T) {
	limits := DefaultLimits
	limits.Timeout = 200 * time.Millisecond
	host := Host{Names: []string{"ask"}, Call: func(ctx context.Context, name, args string) (string, error) {
		resume := PauseClock(ctx)
		defer resume()
		time.Sleep(400 * time.Millisecond)
		return "approved", nil
	}}
	result, err := Run(context.Background(), `async () => await goat.ask({})`, host, limits)
	if err != nil || result.Value != "approved" {
		t.Fatalf("got %q %v", result.Value, err)
	}
}

func TestCancelIsAnInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := Run(ctx, "while (true) {}", echoHost(nil), DefaultLimits)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err = Run(ctx, `async () => { try { await goat.slow({}) } catch (e) {} return 1 }`, echoHost(nil), DefaultLimits)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel during a host call: %v", err)
	}
}

func TestStackOverflowIsSurvivable(t *testing.T) {
	_, err := runCode(t, "function f(n) { return f(n + 1) }\nreturn f(0)")
	if err == nil || !strings.Contains(err.Error(), "call stack") {
		t.Fatalf("got %v", err)
	}
	_, err = runCode(t, "function f(n) { return f(n + 1) }\ntry { f(0) } catch (e) { return 'caught' }")
	if err == nil || !strings.Contains(err.Error(), "call stack") {
		t.Fatalf("an overflow should not be catchable: %v", err)
	}
}

func TestMemoryWatchdog(t *testing.T) {
	limits := DefaultLimits
	limits.MaxHeapGrowth = 64 << 20
	_, err := Run(context.Background(), `async () => { const a = []; while (true) a.push("x".repeat(1000) + a.length); }`, echoHost(nil), limits)
	if err == nil || !strings.Contains(err.Error(), "memory") {
		t.Fatalf("got %v", err)
	}
}

func TestNeverSettlingPromise(t *testing.T) {
	_, err := runCode(t, `async () => { await new Promise(() => {}); }`)
	if err == nil || !strings.Contains(err.Error(), "never settles") {
		t.Fatalf("got %v", err)
	}
}

func TestOnlyGoatAndConsoleAreInstalled(t *testing.T) {
	result, err := runCode(t, `async () => [typeof require, typeof process, typeof fetch, typeof setTimeout, typeof XMLHttpRequest, typeof goat.run_code, typeof goat.read_file].join(",")`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Value != "undefined,undefined,undefined,undefined,undefined,undefined,function" {
		t.Errorf("globals: %s", result.Value)
	}
}

func TestSummary(t *testing.T) {
	for _, tc := range []struct {
		code, want string
		lines      int
	}{
		{"async () => {\n  const files = await goat.find_files({ path: \".\" });\n  return files;\n}", `const files = await goat.find_files({ path: "." });`, 4},
		{"```js\nasync function main() {\n// count TODOs\nreturn 1\n}\n```", "// count TODOs", 4},
		{"return 1", "return 1", 1},
		{"async () => {", "async () => {", 1},
	} {
		got, lines := Summary(tc.code)
		if got != tc.want || lines != tc.lines {
			t.Errorf("Summary(%q) = %q, %d; want %q, %d", tc.code, got, lines, tc.want, tc.lines)
		}
	}
}
