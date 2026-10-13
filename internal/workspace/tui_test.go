package workspace

import (
	"strings"
	"testing"
)

func TestParseTUIKeys(t *testing.T) {
	for _, test := range []struct{ keys, want, wantAppCursor string }{
		{"hello<Enter>", "hello\r", ""},
		{"<C-c><c-D><C-[>", "\x03\x04\x1b", ""},
		{"<M-x><A-b>", "\x1bx\x1bb", ""},
		{"a<lt>b>c", "a<b>c", ""},
		{"<Tab><S-Tab><Esc><Backspace><Space>", "\t\x1b[Z\x1b\x7f ", ""},
		{"<Up><down><Left><RIGHT>", "\x1b[A\x1b[B\x1b[D\x1b[C", "\x1bOA\x1bOB\x1bOD\x1bOC"},
		{"<PgUp><PgDn><Delete><F1><F12>", "\x1b[5~\x1b[6~\x1b[3~\x1bOP\x1b[24~", ""},
		{"", "", ""},
	} {
		keys, err := parseTUIKeys(test.keys)
		if err != nil {
			t.Fatalf("%q: %v", test.keys, err)
		}
		var normal, appCursor strings.Builder
		for _, key := range keys {
			normal.WriteString(key.encode(false))
			appCursor.WriteString(key.encode(true))
		}
		if normal.String() != test.want {
			t.Errorf("%q: got %q, want %q", test.keys, normal.String(), test.want)
		}
		if test.wantAppCursor == "" {
			test.wantAppCursor = test.want
		}
		if appCursor.String() != test.wantAppCursor {
			t.Errorf("%q in application cursor mode: got %q, want %q", test.keys, appCursor.String(), test.wantAppCursor)
		}
	}
}

func TestParseTUIKeysRefusesWhatItCannotType(t *testing.T) {
	for keys, want := range map[string]string{
		"<Entr>":   "unknown key <Entr>",
		"if a < b": "no closing >",
		"<C-1>":    "unknown key <C-1>",
		"<>":       "unknown key <>",
	} {
		if err := ParseTUIKeys(keys); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: expected %q, got %v", keys, want, err)
		}
	}
}

func TestCheckTUIRun(t *testing.T) {
	run := TUIRun{Command: "top"}
	if err := checkTUIRun(&run); err != nil || run.Cols != DefaultTUICols || run.Rows != DefaultTUIRows {
		t.Fatalf("expected the default size, got %dx%d (%v)", run.Cols, run.Rows, err)
	}
	for _, test := range []struct {
		run  TUIRun
		want string
	}{
		{TUIRun{Command: "  "}, "missing command"},
		{TUIRun{Command: "top", Cols: 10}, "out of range"},
		{TUIRun{Command: "top", Rows: 500}, "out of range"},
		{TUIRun{Command: "top", Steps: make([]TUIStep, maxTUISteps+1)}, "more than"},
		{TUIRun{Command: "top", Steps: []TUIStep{{}, {Keys: "<Nope>"}}}, "step 2: keys: unknown key"},
		{TUIRun{Command: "top", Steps: []TUIStep{{WaitMS: -1}}}, "wait_ms"},
		{TUIRun{Command: "top", Steps: []TUIStep{{WaitMS: 60000}}}, "wait_ms"},
	} {
		if err := CheckTUIRun(test.run); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%+v: expected %q, got %v", test.run, test.want, err)
		}
	}
}

func TestScreenText(t *testing.T) {
	if got := screenText("hello   \n  world\x00\x00\n     \n\n"); got != "hello\n  world" {
		t.Errorf("got %q", got)
	}
	if got := screenText("   \n   \n"); got != "(blank screen)" {
		t.Errorf("got %q", got)
	}
}

func TestTUIApprovalTextShowsWhatIsTyped(t *testing.T) {
	got := TUIApprovalText("bash", []TUIStep{{Keys: "rm -rf build<Enter>"}, {WaitFor: "$"}, {Keys: "exit<Enter>"}})
	if got != "bash\n# then types: rm -rf build<Enter>\n# then types: exit<Enter>" {
		t.Errorf("got %q", got)
	}
}
