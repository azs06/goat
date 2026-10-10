package provider

import (
	"testing"
)

func TestParseReasoningEffortDefaultsToMedium(t *testing.T) {
	effort, err := ParseEffort("")
	if err != nil {
		t.Fatalf("parse reasoning effort: %v", err)
	}
	if effort != EffortMedium {
		t.Fatalf("expected medium reasoning effort, got %q", effort)
	}
}

func TestParseReasoningEffortAcceptsAllLevels(t *testing.T) {
	for _, value := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
		if _, err := ParseEffort(value); err != nil {
			t.Errorf("%s: %v", value, err)
		}
	}
	if _, err := ParseEffort("extreme"); err == nil {
		t.Error("expected unknown effort to fail")
	}
}
