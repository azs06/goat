// Package terminal controls the terminal goat runs in: echo and line buffering on
// stdin, and the size of the terminal stdout writes to. On platforms without termios
// support the controls do nothing and sizes are unknown.
package terminal

import (
	"fmt"
	"os"
)

// Warnf reports a problem that does not fail the operation, such as a terminal state
// that could not be restored. The CLI replaces it to route warnings through its own
// styling.
var Warnf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[warn] "+format+"\n", args...)
}
