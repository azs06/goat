package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

type lineResult struct {
	text string
	err  error
}

// lineReader reads input lines without blocking callers that need to give up when their
// context is cancelled. A read abandoned that way stays in flight, and the next
// ReadLine receives its line, so typed input is never dropped. It is not safe for
// concurrent use.
type lineReader struct {
	reader   *bufio.Reader
	results  chan lineResult
	inFlight bool
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{
		reader:  bufio.NewReader(r),
		results: make(chan lineResult, 1),
	}
}

// ReadLine returns the next line without its line ending, or io.EOF once input is closed.
func (r *lineReader) ReadLine(ctx context.Context) (string, error) {
	if !r.inFlight {
		r.inFlight = true
		go func() {
			line, err := r.reader.ReadString('\n')
			if errors.Is(err, io.EOF) && line != "" {
				err = nil
			}
			r.results <- lineResult{text: strings.TrimRight(line, "\r\n"), err: err}
		}()
	}

	select {
	case result := <-r.results:
		r.inFlight = false
		return result.text, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

var errBashCommandDeclined = errors.New("the user declined to run this command")

// bashApprover asks the user before each run_bash command unless autoApprove is set,
// either up front or by answering "always". Without an interactive terminal there is
// nobody to ask, and reading stdin would consume the next scripted prompt, so commands
// are declined.
type bashApprover struct {
	input       *lineReader
	interactive bool
	autoApprove bool
}

func (a *bashApprover) approve(ctx context.Context, command, workdir string) error {
	if a.autoApprove {
		return nil
	}
	if !a.interactive {
		return fmt.Errorf("%w: approval needs an interactive terminal; set GOAT_AUTO_APPROVE_BASH=1 to allow commands", errBashCommandDeclined)
	}
	if strings.TrimSpace(workdir) == "" {
		workdir = "."
	}

	resume := suspendInputGuard()
	defer resume()

	fmt.Printf("\nAllow run_bash in %s?\n%s\n[y]es / [n]o / [a]lways this session (or type feedback to decline): ", escapeControlChars(workdir), indentBlock(escapeControlChars(command), "    "))
	answer, err := a.input.ReadLine(ctx)
	if err != nil {
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
		a.autoApprove = true
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
