//go:build !unix

package workspace

import (
	"context"
	"errors"
)

// TUIAvailable reports whether run_tui can run here: it needs Unix pseudo-terminals.
const TUIAvailable = false

// RunTUI is not available without Unix pseudo-terminals.
func RunTUI(context.Context, TUIRun) (string, error) {
	return "", errors.New("run_tui needs a Unix pseudo-terminal and is not available on this system")
}
