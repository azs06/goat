package tools

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/azs06/goat/internal/provider"
)

// A subagent's command waits for main to ask about it, and the user can turn plan mode
// on meanwhile: the command must not run once approved.
func TestPlanModeTurnedOnDuringAnApprovalStopsTheCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	var plan atomic.Bool
	opts := Options{
		LivePlanMode: plan.Load,
		ApproveBash: func(context.Context, string, string) error {
			plan.Store(true) // the user typed /plan on before answering
			return nil
		},
	}
	call := provider.ToolCall{Name: "run_bash", Arguments: `{"command":"touch made"}`}
	if _, err := Execute(context.Background(), call, opts); !errors.Is(err, ErrPlanMode) {
		t.Fatalf("got %v, want ErrPlanMode", err)
	}
	if _, err := os.Stat("made"); err == nil {
		t.Fatal("the command ran in plan mode")
	}

	plan.Store(false)
	tui := provider.ToolCall{Name: RunTUI, Arguments: `{"command":"touch made"}`}
	if _, err := Execute(context.Background(), tui, opts); !errors.Is(err, ErrPlanMode) {
		t.Fatalf("run_tui: got %v, want ErrPlanMode", err)
	}
	if _, err := os.Stat("made"); err == nil {
		t.Fatal("run_tui ran in plan mode")
	}
}
