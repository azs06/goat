// Package workspace holds goat's file tools: path resolution confined to the working
// directory, reads, searches, previews, and mutations recorded for undo, plus the
// environment and process setup run_bash uses.
package workspace

import (
	"fmt"
	"os"
)

// Warnf reports a problem that does not fail the operation, such as a change that
// could not be recorded for undo. The CLI replaces it to route warnings through its
// own styling.
var Warnf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[warn] "+format+"\n", args...)
}
