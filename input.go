package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/workspace"
)

type keyResult struct {
	key string
	err error
}

// lineReader reads input without blocking callers that need to give up when their
// context is cancelled. It reads one key at a time, so the same reader serves both
// whole lines (ReadLine) and the prompt editor's keystrokes (readKey). A read abandoned
// on cancellation stays in flight, and the next read receives its key, and a line
// cancelled halfway is kept for the next ReadLine, so typed input is never dropped. It
// is not safe for concurrent use.
type lineReader struct {
	reader   *bufio.Reader
	results  chan keyResult
	inFlight bool
	// partial is the start of a line whose ReadLine was cancelled before its newline.
	partial string
	// lastTerminated reports whether the last line read ended with a newline; a line
	// ended by Ctrl-D at a terminal does not, so no newline was echoed after it.
	lastTerminated bool
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{
		reader:  bufio.NewReader(r),
		results: make(chan keyResult, 1),
	}
}

// ReadLine returns the next line without its line ending, or io.EOF once input is closed.
func (r *lineReader) ReadLine(ctx context.Context) (string, error) {
	for {
		key, err := r.readKey(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) && r.partial != "" {
				return r.takeLine(false), nil
			}
			return "", err
		}
		if key == "\n" {
			return r.takeLine(true), nil
		}
		r.partial += key
	}
}

func (r *lineReader) takeLine(terminated bool) string {
	line := strings.TrimSuffix(r.partial, "\r")
	r.partial = ""
	r.lastTerminated = terminated
	return line
}

// readKey returns the next key: one rune, or a whole escape sequence such as the
// "\x1b[A" an arrow key sends.
func (r *lineReader) readKey(ctx context.Context) (string, error) {
	if !r.inFlight {
		r.inFlight = true
		go func() {
			key, err := readKeyFrom(r.reader)
			r.results <- keyResult{key: key, err: err}
		}()
	}

	select {
	case result := <-r.results:
		r.inFlight = false
		return result.key, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// readKeyFrom reads one rune, or one escape sequence when an ESC arrives with more
// bytes right behind it. A lone ESC (the Escape key) has nothing buffered after it.
func readKeyFrom(br *bufio.Reader) (string, error) {
	first, _, err := br.ReadRune()
	if err != nil {
		return "", err
	}
	if first != 0x1b || br.Buffered() == 0 {
		return string(first), nil
	}
	next, _, err := br.ReadRune()
	if err != nil {
		return string(first), nil
	}
	seq := []rune{first, next}
	switch next {
	case '[': // CSI: parameters, then a final byte in @..~
		for br.Buffered() > 0 {
			ch, _, err := br.ReadRune()
			if err != nil {
				break
			}
			seq = append(seq, ch)
			if ch >= 0x40 && ch <= 0x7e {
				break
			}
		}
	case 'O': // SS3: one more byte
		if br.Buffered() > 0 {
			if ch, _, err := br.ReadRune(); err == nil {
				seq = append(seq, ch)
			}
		}
	}
	return string(seq), nil
}

var errBashCommandDeclined = errors.New("the user declined to run this command")

// bashApprover asks the user before each run_bash command unless autoApprove is set,
// either up front or by answering "always". Without an interactive terminal there is
// nobody to ask, and reading stdin would consume the next scripted prompt, so commands
// are declined.
type bashApprover struct {
	input       *lineReader
	interactive bool
	// autoApprove is atomic because turns running beside the main one read it.
	autoApprove atomic.Bool
}

func (a *bashApprover) approve(ctx context.Context, command, workdir string) error {
	touchesState := mentionsStateDir(command, workdir)
	touchesKeys := mentionsConfigDir(command, workdir)
	if a.autoApprove.Load() && !touchesState && !touchesKeys {
		return nil
	}
	if !a.interactive {
		if touchesState {
			return fmt.Errorf("%w: commands that touch goat's state directory, with its history and undo snapshots, always need the user's approval", errBashCommandDeclined)
		}
		if touchesKeys {
			return fmt.Errorf("%w: commands that mention goat's config directory, which holds the user's API keys, always need the user's approval", errBashCommandDeclined)
		}
		return fmt.Errorf("%w: approval needs an interactive terminal; set GOAT_AUTO_APPROVE_BASH=1 to allow commands", errBashCommandDeclined)
	}
	if strings.TrimSpace(workdir) == "" {
		workdir = "."
	}

	resume := suspendInputGuard()
	defer resume()

	if touchesState {
		fmt.Println("\n" + formatWarning("This command may touch goat's state directory, "+displayPath(workspace.StateHome())+", with the history and undo snapshots; run_bash changes are not undoable."))
	}
	if touchesKeys {
		fmt.Println("\n" + formatWarning("This command mentions goat's config directory, where "+displayPath(globalEnvPath())+" may hold your API keys."))
	}
	if style.Enabled() {
		fmt.Print(renderApprovalCard(command, workdir, terminalWidth()))
	} else {
		fmt.Printf("\nAllow run_bash in %s?\n%s\n[y]es / [n]o / [a]lways this session (or type feedback to decline): ", escapeControlChars(workdir), indentBlock(escapeControlChars(command), "    "))
	}
	answer, err := a.input.ReadLine(ctx)
	if err != nil {
		if style.Enabled() && errors.Is(err, context.Canceled) {
			// Redraw the prompt row to wipe the "^C" the terminal echoed onto it.
			fmt.Print("\r\x1b[K" + renderApprovalPrompt())
		}
		fmt.Println()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: input closed before approval", errBashCommandDeclined)
		}
		return err
	}

	switch normalized := strings.ToLower(strings.TrimSpace(answer)); normalized {
	case "y", "yes":
		return nil
	case "a", "always":
		a.autoApprove.Store(true)
		return nil
	case "", "n", "no":
		return errBashCommandDeclined
	default:
		return fmt.Errorf("%w; their feedback: %s", errBashCommandDeclined, strings.TrimSpace(answer))
	}
}

// escapeControlChars makes terminal control characters visible, so a command cannot use
// escape sequences or carriage returns to disguise itself in the approval prompt.
func escapeControlChars(text string) string {
	var builder strings.Builder
	for _, r := range text {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			builder.WriteRune(r)
			continue
		}
		fmt.Fprintf(&builder, "\\x%02x", r)
	}
	return builder.String()
}
