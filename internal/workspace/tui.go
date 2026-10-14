package workspace

import (
	"fmt"
	"strings"
	"time"
)

// run_tui runs a command in a pseudo-terminal, types keys into it, and returns what the
// screen shows, so the agent can check a terminal UI the way a person would see it.
// This file holds what every platform shares: the key syntax, the limits and the screen
// text. tui_unix.go runs the program.

const (
	// DefaultTUICols and DefaultTUIRows are the terminal size when a call gives none.
	DefaultTUICols = 100
	DefaultTUIRows = 30

	minTUICols, maxTUICols = 20, 300
	minTUIRows, maxTUIRows = 5, 100
	// maxTUISteps bounds the steps one call can take.
	maxTUISteps = 50
	// maxTUIWait is the longest one step may wait, and the wait for wait_for text when the
	// step gives no wait_ms.
	maxTUIWait     = 30 * time.Second
	defaultTUIWait = 10 * time.Second
)

// tuiTimeout is how long a run_tui call may take in all before its program is killed.
// It is a variable so tests can shorten it.
var tuiTimeout = 60 * time.Second

// TUIStep is one step of a run_tui call: keys to type, then an optional wait, then an
// optional snapshot of the screen.
type TUIStep struct {
	// Keys are typed into the terminal; see ParseTUIKeys for the syntax.
	Keys string `json:"keys,omitempty"`
	// WaitFor waits until the screen shows this text, for up to WaitMS or 10 seconds.
	WaitFor string `json:"wait_for,omitempty"`
	// WaitMS is how long to wait: a pause on its own, or the limit for WaitFor.
	WaitMS int `json:"wait_ms,omitempty"`
	// Snapshot records the screen after this step.
	Snapshot bool `json:"snapshot,omitempty"`
	// Then, when set, runs once the step is done, before the next one: for Go callers
	// such as tests, which act outside the program at a point the screen shows. The
	// run_tui tool never sets it.
	Then func() `json:"-"`
}

// TUIRun describes one run_tui call.
type TUIRun struct {
	Command    string
	Workdir    string
	Cols, Rows int
	Steps      []TUIStep
}

// tuiKey is one key in a keys string: literal text, or a named key whose bytes can
// depend on the terminal's mode (arrow keys in application cursor mode).
type tuiKey struct {
	text string
	name string
}

// namedTUIKeys are the keys written as <Name>, with the bytes a terminal sends for them.
// Names are matched without regard to case.
var namedTUIKeys = map[string]string{
	"enter": "\r", "return": "\r", "cr": "\r",
	"tab": "\t", "s-tab": "\x1b[Z", "backtab": "\x1b[Z",
	"esc": "\x1b", "escape": "\x1b",
	"bs": "\x7f", "backspace": "\x7f",
	"space": " ", "lt": "<",
	"up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D",
	"home": "\x1b[H", "end": "\x1b[F",
	"pgup": "\x1b[5~", "pageup": "\x1b[5~", "pgdn": "\x1b[6~", "pagedown": "\x1b[6~",
	"del": "\x1b[3~", "delete": "\x1b[3~", "insert": "\x1b[2~",
	"f1": "\x1bOP", "f2": "\x1bOQ", "f3": "\x1bOR", "f4": "\x1bOS",
	"f5": "\x1b[15~", "f6": "\x1b[17~", "f7": "\x1b[18~", "f8": "\x1b[19~",
	"f9": "\x1b[20~", "f10": "\x1b[21~", "f11": "\x1b[23~", "f12": "\x1b[24~",
}

// cursorKeys are sent as ESC O x instead of ESC [ x while the program has turned on
// application cursor keys (DECCKM), as full-screen programs such as vim and less do.
var cursorKeys = map[string]string{"up": "\x1bOA", "down": "\x1bOB", "right": "\x1bOC", "left": "\x1bOD", "home": "\x1bOH", "end": "\x1bOF"}

// ParseTUIKeys checks a keys string. Text is typed as it is; special keys go in angle
// brackets: <Enter>, <Tab>, <S-Tab>, <Esc>, <Backspace>, <Space>, <Up>, <Down>, <Left>,
// <Right>, <Home>, <End>, <PgUp>, <PgDn>, <Delete>, <Insert>, <F1>-<F12>, Ctrl with
// <C-x> (such as <C-c>), Alt with <M-x>, and <lt> for a literal "<". An unknown name is
// an error, so a misspelled key is never typed as text.
func ParseTUIKeys(keys string) error {
	_, err := parseTUIKeys(keys)
	return err
}

func parseTUIKeys(keys string) ([]tuiKey, error) {
	var parsed []tuiKey
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			parsed = append(parsed, tuiKey{text: text.String()})
			text.Reset()
		}
	}
	for rest := keys; rest != ""; {
		open := strings.IndexByte(rest, '<')
		if open < 0 {
			text.WriteString(rest)
			break
		}
		text.WriteString(rest[:open])
		end := strings.IndexByte(rest[open:], '>')
		if end < 0 {
			return nil, fmt.Errorf("keys: %q has a < with no closing >; write a literal < as <lt>", keys)
		}
		name := rest[open+1 : open+end]
		rest = rest[open+end+1:]
		key, err := parseNamedKey(name)
		if err != nil {
			return nil, err
		}
		if key.name == "" {
			text.WriteString(key.text)
			continue
		}
		flush()
		parsed = append(parsed, key)
	}
	flush()
	return parsed, nil
}

func parseNamedKey(name string) (tuiKey, error) {
	lower := strings.ToLower(name)
	if _, ok := cursorKeys[lower]; ok {
		return tuiKey{name: lower}, nil
	}
	if bytes, ok := namedTUIKeys[lower]; ok {
		return tuiKey{text: bytes}, nil
	}
	if len(name) == 3 && name[1] == '-' {
		switch c := name[2]; lower[0] {
		case 'c':
			switch {
			case c >= 'a' && c <= 'z':
				return tuiKey{text: string(rune(c - 'a' + 1))}, nil
			case c >= 'A' && c <= 'Z':
				return tuiKey{text: string(rune(c - 'A' + 1))}, nil
			case c == '[':
				return tuiKey{text: "\x1b"}, nil
			case c == '\\':
				return tuiKey{text: "\x1c"}, nil
			}
		case 'm', 'a':
			if c >= ' ' && c < 0x7f {
				return tuiKey{text: "\x1b" + string(c)}, nil
			}
		}
	}
	return tuiKey{}, fmt.Errorf("keys: unknown key <%s>; use names such as <Enter>, <Esc>, <Up>, <C-c> or <M-x>, and <lt> for a literal <", name)
}

// encode is the bytes a terminal sends for k, given whether the program has application
// cursor keys on.
func (k tuiKey) encode(appCursor bool) string {
	if k.name == "" {
		return k.text
	}
	if appCursor {
		return cursorKeys[k.name]
	}
	return namedTUIKeys[k.name]
}

// checkTUIRun validates a call before anything runs and fills in the default size.
func checkTUIRun(run *TUIRun) error {
	if strings.TrimSpace(run.Command) == "" {
		return fmt.Errorf("missing command")
	}
	if run.Cols == 0 {
		run.Cols = DefaultTUICols
	}
	if run.Rows == 0 {
		run.Rows = DefaultTUIRows
	}
	if run.Cols < minTUICols || run.Cols > maxTUICols || run.Rows < minTUIRows || run.Rows > maxTUIRows {
		return fmt.Errorf("terminal size %dx%d is out of range: cols %d-%d, rows %d-%d", run.Cols, run.Rows, minTUICols, maxTUICols, minTUIRows, maxTUIRows)
	}
	if len(run.Steps) > maxTUISteps {
		return fmt.Errorf("%d steps is more than the %d one call may take", len(run.Steps), maxTUISteps)
	}
	for i, step := range run.Steps {
		if _, err := parseTUIKeys(step.Keys); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
		if step.WaitMS < 0 || time.Duration(step.WaitMS)*time.Millisecond > maxTUIWait {
			return fmt.Errorf("step %d: wait_ms must be between 0 and %d", i+1, maxTUIWait.Milliseconds())
		}
	}
	return nil
}

// CheckTUIRun validates a run_tui call, so the user is never asked to approve one that
// cannot run.
func CheckTUIRun(run TUIRun) error {
	return checkTUIRun(&run)
}

// screenText turns the emulator's dump, one line per row, into what the model reads:
// trailing spaces and trailing blank rows dropped.
func screenText(dump string) string {
	lines := strings.Split(strings.TrimSuffix(dump, "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \x00")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return "(blank screen)"
	}
	return strings.Join(lines, "\n")
}

// TUIApprovalText is what the user approves for a run_tui call: the command, then each
// step's keys, since what is typed into a shell or program can do as much as a command.
func TUIApprovalText(command string, steps []TUIStep) string {
	text := command
	for _, step := range steps {
		if step.Keys != "" {
			text += "\n# then types: " + step.Keys
		}
	}
	return text
}
