package subagent

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// convBox stands in for package main's conversation state.
type convBox struct {
	data []byte
}

func TestEvictedAgentsReleaseTheirConversations(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	var finalized atomic.Int64
	m := NewManager(Config{MaxRunning: 1, MaxLive: 4}, func(ctx context.Context, a *Agent, prompt string) (string, error) {
		box := &convBox{data: make([]byte, 1<<20)}
		for i := range box.data {
			box.data[i] = byte(i)
		}
		runtime.SetFinalizer(box, func(*convBox) { finalized.Add(1) })
		a.SetConv(box)
		a.Ring().Append(Event{Kind: EventText, Text: prompt})
		return "done", nil
	})
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	const agents = 200
	for range agents {
		var a *Agent
		eventually(t, "a free slot", func() bool {
			var err error
			a, err = m.Spawn(SpawnOptions{Label: "mem", Prompt: "fill"})
			return err == nil
		})
		waitDone(t, a)
	}
	eventually(t, "every agent dismissed", func() bool {
		m.DismissFinished()
		_, _, total := m.Counts()
		return total == 0
	})

	deadline := time.Now().Add(10 * time.Second)
	for finalized.Load() < agents && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if got := finalized.Load(); got != agents {
		t.Fatalf("%d of %d conversations collected", got, agents)
	}
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if delta := int64(after.HeapAlloc) - int64(before.HeapAlloc); delta > 10<<20 {
		t.Fatalf("heap grew by %d MB", delta>>20)
	}
	if infos := m.List(); len(infos) != m.Config().MaxEvicted || !infos[0].Evicted {
		t.Fatalf("%d summaries kept", len(infos))
	}
	shutdown(t, m)
}
