package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// recordConcurrentMutations runs workers goroutines that each record perWorker
// writes to files of their own, while during runs alongside them.
func recordConcurrentMutations(t *testing.T, workspace string, workers, perWorker int, during func()) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				name := fmt.Sprintf("w%d-%d.txt", w, i)
				err := withRecordedMutation("write_file", []string{name}, func() error {
					return os.WriteFile(filepath.Join(workspace, name), []byte(name), 0644)
				})
				if err != nil {
					errs <- fmt.Errorf("%s: %w", name, err)
				}
			}
		}()
	}
	if during != nil {
		during()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func raiseMaxRecordedOperations(t *testing.T, limit int) {
	t.Helper()
	previous := maxRecordedOperations
	maxRecordedOperations = limit
	t.Cleanup(func() { maxRecordedOperations = previous })
}

func TestConcurrentMutationsRecordEveryOperation(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	raiseMaxRecordedOperations(t, 1000)

	recordConcurrentMutations(t, workspace, 8, 50, nil)

	records, err := loadOperationRecords(false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(records) != 400 {
		t.Fatalf("expected 400 records, got %d", len(records))
	}
	ids := map[string]bool{}
	for _, record := range records {
		if ids[record.ID] {
			t.Fatalf("duplicate operation id %s", record.ID)
		}
		ids[record.ID] = true
		if !operationSnapshotExists(record) {
			t.Errorf("record %s has no snapshot", record.ID)
		}
	}
}

func TestUndoDuringConcurrentMutationsKeepsLogIntact(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	raiseMaxRecordedOperations(t, 1000)

	undone := 0
	recordConcurrentMutations(t, workspace, 4, 25, func() {
		for range 20 {
			message, err := UndoLastChange()
			if err != nil {
				t.Errorf("undo: %v", err)
				continue
			}
			if message != "No recorded operations available to undo." {
				undone++
			}
		}
	})

	records, err := loadOperationRecords(false)
	if err != nil {
		t.Fatalf("expected a readable log, got %v", err)
	}
	if len(records) != 100 {
		t.Fatalf("expected 100 records, got %d", len(records))
	}
	ids := map[string]bool{}
	marked := 0
	for _, record := range records {
		if ids[record.ID] {
			t.Fatalf("duplicate operation id %s", record.ID)
		}
		ids[record.ID] = true
		if record.UndoneAt != "" {
			marked++
			if record.Skipped {
				t.Errorf("record %s was skipped", record.ID)
			}
		}
	}
	if marked != undone {
		t.Fatalf("expected %d records marked undone, got %d", undone, marked)
	}
}
