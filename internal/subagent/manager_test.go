package subagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// call is one runner invocation the test answers.
type call struct {
	a      *Agent
	prompt string
	reply  chan reply
}

type reply struct {
	text  string
	err   error
	panic bool
}

// fakeRunner hands every turn to the test on calls and returns what the test replies.
// When obeyCtx is set a turn also ends when its context does.
func fakeRunner(calls chan<- call, obeyCtx bool) Runner {
	return func(ctx context.Context, a *Agent, prompt string) (string, error) {
		c := call{a: a, prompt: prompt, reply: make(chan reply, 1)}
		calls <- c
		var r reply
		if obeyCtx {
			select {
			case r = <-c.reply:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		} else {
			r = <-c.reply
		}
		if r.panic {
			panic("boom")
		}
		return r.text, r.err
	}
}

func instantRunner(ctx context.Context, a *Agent, prompt string) (string, error) {
	return "done: " + prompt, nil
}

func next(t *testing.T, calls <-chan call) call {
	t.Helper()
	select {
	case c := <-calls:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no runner call")
		return call{}
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitDone(t *testing.T, a *Agent) {
	t.Helper()
	select {
	case <-a.TurnDone():
	case <-time.After(5 * time.Second):
		t.Fatalf("%s's turn did not end", a.ID)
	}
}

// settle waits for a's turn to end and its goroutine to exit, after which it can be
// evicted.
func settle(t *testing.T, a *Agent) {
	t.Helper()
	waitDone(t, a)
	eventually(t, a.ID+"'s goroutine to exit", func() bool { return goroutines(a) == 0 })
}

func goroutines(a *Agent) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.goroutines
}

func shutdown(t *testing.T, m *Manager) {
	t.Helper()
	if left := m.StopAll(5 * time.Second); left != nil {
		t.Fatalf("stragglers %v", left)
	}
}

func TestSpawnFiftyAgentsAndStopAllLeavesNoGoroutines(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := NewManager(Config{MaxRunning: 50, MaxLive: 100}, instantRunner)
	for i := range 50 {
		if _, err := m.Spawn(SpawnOptions{Label: fmt.Sprint("task ", i), Prompt: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := m.Wait(context.Background(), nil, true, 5*time.Second)
	if err != nil || res.TimedOut || len(res.Running) != 0 {
		t.Fatalf("wait: %+v %v", res, err)
	}
	for _, info := range m.List() {
		if info.State != Idle || info.Turns != 1 || info.ReportHead != "done: "+strings.TrimPrefix(info.Label, "task ") {
			t.Fatalf("got %+v", info)
		}
	}
	if running, _, total := m.Counts(); running != 0 || total != 50 {
		t.Fatalf("counts %d %d", running, total)
	}
	shutdown(t, m)
	if _, err := m.Spawn(SpawnOptions{Prompt: "x"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("spawn after StopAll: %v", err)
	}
}

func TestStopMidTurnIsFinalForTheModelAndIgnoresTheLateResult(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 4)
	m := NewManager(Config{}, fakeRunner(calls, false))
	a, err := m.Spawn(SpawnOptions{Label: "scan", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	c := next(t, calls)
	if err := m.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	if a.State() != Stopped {
		t.Fatalf("state %v", a.State())
	}
	waitDone(t, a)
	if _, err := m.Send(a.ID, "more", false); !errors.Is(err, ErrStopped) {
		t.Fatalf("model send to a stopped agent: %v", err)
	}
	// The runner ignores its context and answers late: the stale generation is ignored.
	c.reply <- reply{text: "late"}
	eventually(t, "the old turn to exit", func() bool { return goroutines(a) == 0 })
	if a.State() != Stopped || a.Report() != "" || !errors.Is(a.Err(), ErrStopped) {
		t.Fatalf("got %v %q %v", a.State(), a.Report(), a.Err())
	}
	// The user can resume it.
	started, err := m.Send(a.ID, "resume", true)
	if err != nil || !started {
		t.Fatalf("user resume: %v %v", started, err)
	}
	c = next(t, calls)
	if c.prompt != "resume" {
		t.Fatalf("prompt %q", c.prompt)
	}
	c.reply <- reply{text: "back"}
	waitDone(t, a)
	if a.State() != Idle || a.Report() != "back" {
		t.Fatalf("got %v %q", a.State(), a.Report())
	}
	shutdown(t, m)
}

func TestUserResumeWaitsForAStrayRunner(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 4)
	m := NewManager(Config{}, fakeRunner(calls, false))
	a, _ := m.Spawn(SpawnOptions{Prompt: "go"})
	c := next(t, calls)
	_ = m.Stop(a.ID)
	started, err := m.Send(a.ID, "resume", true)
	if err != nil || started {
		t.Fatalf("resume while the old runner runs: %v %v", started, err)
	}
	c.reply <- reply{text: "late"}
	c = next(t, calls)
	if c.prompt != "resume" {
		t.Fatalf("prompt %q", c.prompt)
	}
	c.reply <- reply{text: "ok"}
	waitDone(t, a)
	shutdown(t, m)
}

func TestSendRacingTheEndOfATurnContinuesOnTheSameGoroutine(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	var m *Manager
	var prompts []string
	var turnDone <-chan struct{}
	var sameChannel atomic.Bool
	m = NewManager(Config{}, func(ctx context.Context, a *Agent, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		if len(prompts) == 1 {
			turnDone = a.TurnDone()
			// The message lands after the runner's last look at the inbox.
			if started, err := m.Send(a.ID, "late message", false); err != nil || started {
				return "", fmt.Errorf("send: %v %v", started, err)
			}
			return "first", nil
		}
		sameChannel.Store(a.TurnDone() == turnDone)
		return "second", nil
	})
	a, err := m.Spawn(SpawnOptions{Prompt: "start"})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, a)
	eventually(t, "the goroutine to exit", func() bool { return goroutines(a) == 0 })
	if len(prompts) != 2 || prompts[1] != "late message" || !sameChannel.Load() {
		t.Fatalf("prompts %q same channel %v", prompts, sameChannel.Load())
	}
	if info := a.Info(); info.Turns != 2 || a.Report() != "second" || info.State != Idle {
		t.Fatalf("got %+v", info)
	}
	shutdown(t, m)
}

func TestInterruptWithMessagesWaitingStartsANewTurn(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 4)
	m := NewManager(Config{}, fakeRunner(calls, true))
	a, _ := m.Spawn(SpawnOptions{Prompt: "go"})
	next(t, calls)
	if started, err := m.Send(a.ID, "steer", false); err != nil || started {
		t.Fatalf("send: %v %v", started, err)
	}
	a.InterruptTurn(ErrParentInterrupted)
	c := next(t, calls)
	if c.prompt != "steer" {
		t.Fatalf("prompt %q", c.prompt)
	}
	if a.Info().Turns != 1 {
		t.Fatalf("turns %d", a.Info().Turns)
	}
	c.reply <- reply{text: "ok"}
	eventually(t, "idle", func() bool { return a.State() == Idle })
	shutdown(t, m)
}

func TestInterruptLeavesTheAgentIdleWithTheCause(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 4)
	m := NewManager(Config{}, fakeRunner(calls, true))
	a, _ := m.Spawn(SpawnOptions{Prompt: "go"})
	next(t, calls)
	ctx, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, func() { a.InterruptTurn(ErrParentInterrupted) })
	cancel()
	waitDone(t, a)
	stop()
	if a.State() != Idle || !errors.Is(a.Err(), ErrParentInterrupted) {
		t.Fatalf("got %v %v", a.State(), a.Err())
	}
	shutdown(t, m)
}

func TestSemaphoreRefusesTheFourthTurnAndAPanicFreesItsSlot(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 8)
	m := NewManager(Config{MaxRunning: 3}, fakeRunner(calls, true))
	var turns []call
	for range 3 {
		if _, err := m.Spawn(SpawnOptions{Prompt: "go"}); err != nil {
			t.Fatal(err)
		}
		turns = append(turns, next(t, calls))
	}
	if _, err := m.Spawn(SpawnOptions{Prompt: "go"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("fourth spawn: %v", err)
	}
	if running, _, total := m.Counts(); running != 3 || total != 3 {
		t.Fatalf("counts %d %d", running, total)
	}
	turns[0].reply <- reply{panic: true}
	waitDone(t, turns[0].a)
	if turns[0].a.State() != Failed || !strings.Contains(fmt.Sprint(turns[0].a.Err()), "subagent panic: boom") {
		t.Fatalf("got %v %v", turns[0].a.State(), turns[0].a.Err())
	}
	var fourth *Agent
	eventually(t, "a free slot", func() bool {
		a, err := m.Spawn(SpawnOptions{Prompt: "go"})
		fourth = a
		return err == nil
	})
	if fourth.ID != "a4" {
		t.Fatalf("id %s", fourth.ID)
	}
	shutdown(t, m)
}

func TestIdleSendWaitsInTheInboxForAFreeSlot(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 8)
	m := NewManager(Config{MaxRunning: 1}, fakeRunner(calls, true))
	a, _ := m.Spawn(SpawnOptions{Prompt: "a"})
	next(t, calls).reply <- reply{text: "ok"}
	waitDone(t, a)
	eventually(t, "slot", func() bool { return goroutines(a) == 0 })
	b, _ := m.Spawn(SpawnOptions{Prompt: "b"})
	cb := next(t, calls)
	started, err := m.Send(a.ID, "again", true)
	if err != nil || started || a.Info().Inbox != 1 {
		t.Fatalf("send with no slot: %v %v %d", started, err, a.Info().Inbox)
	}
	cb.reply <- reply{text: "ok"}
	waitDone(t, b)
	c := next(t, calls)
	if c.a != a || c.prompt != "again" {
		t.Fatalf("kicked %s with %q", c.a.ID, c.prompt)
	}
	c.reply <- reply{}
	shutdown(t, m)
}

func TestFullInboxAndUnknownAgent(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 4)
	m := NewManager(Config{InboxCap: 2}, fakeRunner(calls, true))
	a, _ := m.Spawn(SpawnOptions{Label: "fix test", Prompt: "go"})
	next(t, calls)
	for range 2 {
		if _, err := m.Send(a.ID, "m", false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Send(a.ID, "m", false); !errors.Is(err, ErrInboxFull) {
		t.Fatalf("third message: %v", err)
	}
	if got := a.TakeInbox(); len(got) != 2 {
		t.Fatalf("inbox %q", got)
	}
	_, err := m.Send("a9", "m", false)
	if !errors.Is(err, ErrUnknownAgent) || !strings.Contains(err.Error(), "a1 (fix test)") {
		t.Fatalf("unknown agent: %v", err)
	}
	if _, err := m.Get("a9"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatal(err)
	}
	shutdown(t, m)
}

func TestWaitTimesOutAndHonoursCancel(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 4)
	m := NewManager(Config{}, fakeRunner(calls, true))
	a, _ := m.Spawn(SpawnOptions{Prompt: "go"})
	c := next(t, calls)

	res, err := m.Wait(context.Background(), []string{a.ID}, false, 20*time.Millisecond)
	if err != nil || !res.TimedOut || len(res.Running) != 1 || len(res.Done) != 0 {
		t.Fatalf("timeout: %+v %v", res, err)
	}

	cause := errors.New("ctrl-c")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	start := time.Now()
	if _, err := m.Wait(ctx, nil, true, time.Minute); !errors.Is(err, cause) || time.Since(start) > time.Second {
		t.Fatalf("cancelled wait: %v after %v", err, time.Since(start))
	}

	if _, err := m.Wait(context.Background(), []string{"a7"}, true, time.Second); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("unknown: %v", err)
	}

	c.reply <- reply{text: "report"}
	res, err = m.Wait(context.Background(), []string{a.ID}, true, 5*time.Second)
	if err != nil || len(res.Done) != 1 || res.Done[0].Report != "report" || res.Done[0].State != Idle {
		t.Fatalf("done: %+v %v", res, err)
	}
	if res, err := m.Wait(context.Background(), nil, true, time.Second); err != nil || len(res.Done)+len(res.Running) != 0 {
		t.Fatalf("nothing to wait for: %+v %v", res, err)
	}
	shutdown(t, m)
}

func TestWaitAnyReturnsTheFirstToFinish(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 4)
	m := NewManager(Config{}, fakeRunner(calls, true))
	a, _ := m.Spawn(SpawnOptions{Prompt: "a"})
	ca := next(t, calls)
	b, _ := m.Spawn(SpawnOptions{Prompt: "b"})
	next(t, calls)
	go func() { ca.reply <- reply{text: "a done"} }()
	res, err := m.Wait(context.Background(), []string{a.ID, b.ID}, false, 5*time.Second)
	if err != nil || len(res.Done) != 1 || res.Done[0].ID != a.ID || len(res.Running) != 1 || res.Running[0] != b.ID {
		t.Fatalf("got %+v %v", res, err)
	}
	shutdown(t, m)
}

func TestWaitStopsForApprovalsAndConsumesNotices(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	var m *Manager
	m = NewManager(Config{}, func(ctx context.Context, a *Agent, prompt string) (string, error) {
		a.SetState(WaitingApproval)
		err := m.Broker.Request(ctx, &ApprovalRequest{AgentID: a.ID, Command: "make"})
		a.SetState(Running)
		if err != nil {
			return "", err
		}
		return "built", nil
	})
	a, _ := m.Spawn(SpawnOptions{Label: "build", ParentConv: "c1", Background: true, Prompt: "go"})
	deadline := time.Now().Add(5 * time.Second)
	served := 0
	var res WaitResult
	for {
		var err error
		res, err = m.Wait(context.Background(), nil, true, time.Until(deadline))
		if !errors.Is(err, ErrApprovalPending) {
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if _, waiting, _ := m.Counts(); waiting != 1 {
			t.Fatalf("waiting %d", waiting)
		}
		for req := m.Broker.Next(); req != nil; req = m.Broker.Next() {
			if req.AgentID != a.ID || req.Command != "make" {
				t.Fatalf("request %+v", req)
			}
			req.Answer(nil)
			served++
		}
	}
	if served != 1 || len(res.Done) != 1 || res.Done[0].Report != "built" {
		t.Fatalf("served %d, %+v", served, res)
	}
	if notices := m.TakeNotices(""); len(notices) != 0 {
		t.Fatalf("notice delivered twice: %+v", notices)
	}
	shutdown(t, m)
}

func TestBackgroundNoticesGoToTheirParent(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := NewManager(Config{}, instantRunner)
	a, _ := m.Spawn(SpawnOptions{Label: "one", ParentConv: "c1", Background: true, Prompt: "x"})
	b, _ := m.Spawn(SpawnOptions{Label: "two", ParentConv: "c2", Background: true, Prompt: "y"})
	f, _ := m.Spawn(SpawnOptions{Label: "fg", ParentConv: "c1", Prompt: "z"})
	waitDone(t, a)
	waitDone(t, b)
	waitDone(t, f)
	notices := m.TakeNotices("c1")
	if len(notices) != 1 || notices[0].AgentID != a.ID || !strings.Contains(notices[0].Text, `<agent-report id="a1" label="one">`) || !strings.Contains(notices[0].Text, "done: x") {
		t.Fatalf("c1: %+v", notices)
	}
	if notices := m.TakeNotices(""); len(notices) != 1 || notices[0].ParentConv != "c2" {
		t.Fatalf("rest: %+v", notices)
	}
	shutdown(t, m)
}

func TestStopAllNamesARunnerThatIgnoresItsContext(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 4)
	m := NewManager(Config{}, fakeRunner(calls, false))
	a, _ := m.Spawn(SpawnOptions{Prompt: "go"})
	b, _ := m.Spawn(SpawnOptions{Prompt: "go"})
	ca, cb := next(t, calls), next(t, calls)
	if ca.a != a {
		ca, cb = cb, ca
	}
	cb.reply <- reply{text: "quick"}
	waitDone(t, b)

	start := time.Now()
	left := m.StopAll(50 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("StopAll took %v", elapsed)
	}
	if len(left) != 1 || left[0] != a.ID {
		t.Fatalf("stragglers %v", left)
	}
	if a.State() != Stopped || b.State() != Stopped {
		t.Fatalf("states %v %v", a.State(), b.State())
	}
	if _, err := m.Send(b.ID, "x", true); !errors.Is(err, ErrClosed) {
		t.Fatalf("send after StopAll: %v", err)
	}
	ca.reply <- reply{}
	m.wg.Wait()
}

func TestStopAllAnswersApprovalRequests(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	got := make(chan error, 1)
	m := NewManager(Config{}, func(ctx context.Context, a *Agent, prompt string) (string, error) {
		// A context-blind wait, so only DrainAll can answer it.
		err := m0(a).Broker.Request(context.WithoutCancel(ctx), &ApprovalRequest{AgentID: a.ID})
		got <- err
		return "", err
	})
	m.Spawn(SpawnOptions{Prompt: "go"})
	eventually(t, "the request", func() bool { return m.Broker.Len() == 1 })
	shutdown(t, m)
	if err := <-got; !errors.Is(err, ErrShutdown) {
		t.Fatalf("got %v", err)
	}
}

func m0(a *Agent) *Manager { return a.m }

func TestSetStateAndActivityNudgeChanged(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 4)
	m := NewManager(Config{}, fakeRunner(calls, true))
	a, _ := m.Spawn(SpawnOptions{Prompt: "go"})
	c := next(t, calls)
	drain := func() {
		select {
		case <-m.Changed():
		default:
		}
	}
	drain()
	a.SetActivity("read_file main.go")
	select {
	case <-m.Changed():
	default:
		t.Fatal("no nudge for activity")
	}
	a.SetState(WaitingApproval)
	if a.State() != WaitingApproval {
		t.Fatalf("state %v", a.State())
	}
	a.SetState(Idle) // not the caller's to set
	if a.State() != WaitingApproval || a.Info().Activity != "read_file main.go" {
		t.Fatalf("got %+v", a.Info())
	}
	a.SetState(Running)
	c.reply <- reply{text: "ok"}
	waitDone(t, a)
	a.SetState(Running) // ignored once the turn has ended
	if a.State() != Idle || a.Info().Activity != "" {
		t.Fatalf("got %+v", a.Info())
	}
	shutdown(t, m)
}

func TestEvictionKeepsSummariesAndRefusesBusyAgents(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	calls := make(chan call, 4)
	m := NewManager(Config{MaxLive: 2, MaxEvicted: 2}, fakeRunner(calls, true))
	busy, _ := m.Spawn(SpawnOptions{Label: "busy", Prompt: "go"})
	cBusy := next(t, calls)
	if err := m.Dismiss(busy.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("dismiss running: %v", err)
	}
	for i := range 4 {
		a, err := m.Spawn(SpawnOptions{Label: fmt.Sprint("t", i), Prompt: "go"})
		if err != nil {
			t.Fatal(err)
		}
		next(t, calls).reply <- reply{text: "r"}
		settle(t, a)
	}
	infos := m.List()
	var live, evicted []string
	for _, info := range infos {
		if info.Evicted {
			evicted = append(evicted, info.ID)
		} else {
			live = append(live, info.ID)
		}
	}
	if strings.Join(live, ",") != "a1,a5" || strings.Join(evicted, ",") != "a3,a4" {
		t.Fatalf("live %v evicted %v", live, evicted)
	}
	if _, err := m.Get("a2"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatal(err)
	}
	cBusy.reply <- reply{text: "r"}
	settle(t, busy)
	if got := m.DismissFinished(); strings.Join(got, ",") != "a1,a5" {
		t.Fatalf("dismissed %v", got)
	}
	if _, _, total := m.Counts(); total != 0 {
		t.Fatalf("total %d", total)
	}
	shutdown(t, m)
}

func TestEvictionKeepsUndeliveredReports(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := NewManager(Config{MaxLive: 1}, instantRunner)
	a, _ := m.Spawn(SpawnOptions{Background: true, Prompt: "x"})
	settle(t, a)
	if err := m.Dismiss(a.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("dismiss with a notice: %v", err)
	}
	b, _ := m.Spawn(SpawnOptions{Prompt: "y"})
	settle(t, b)
	if _, _, total := m.Counts(); total != 2 {
		t.Fatalf("total %d", total)
	}
	m.TakeNotices("")
	if err := m.Dismiss(a.ID); err != nil {
		t.Fatal(err)
	}
	shutdown(t, m)
}

func TestReportIsCapped(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := NewManager(Config{ReportCap: 10}, func(ctx context.Context, a *Agent, prompt string) (string, error) {
		return strings.Repeat("é", 20), nil
	})
	a, _ := m.Spawn(SpawnOptions{Prompt: "x"})
	waitDone(t, a)
	if got := a.Report(); got != strings.Repeat("é", 5)+"\n[report truncated at 0 KB; the user can view the full transcript with /agents log a1]" {
		t.Fatalf("report %q", got)
	}
	shutdown(t, m)
}

func TestDefaultConfigReadsMaxRunningFromTheEnvironment(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	t.Setenv("GOAT_MAX_RUNNING_AGENTS", "5")
	if got := DefaultConfig(); got.MaxRunning != 5 || got.MaxLive != 8 || got.InboxCap != 16 || got.ReportCap != 50<<10 || got.StopTimeout != 3*time.Second {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range []string{"", "0", "-2", "many"} {
		t.Setenv("GOAT_MAX_RUNNING_AGENTS", bad)
		if got := DefaultConfig().MaxRunning; got != 3 {
			t.Fatalf("%q: %d", bad, got)
		}
	}
	if got := NewManager(Config{MaxLive: 4}, instantRunner).Config(); got.MaxLive != 4 || got.MaxRunning != 3 || got.RingEvents != 500 {
		t.Fatalf("got %+v", got)
	}
}

// With more agents allowed to run at once (GOAT_MAX_RUNNING_AGENTS), every finished
// background agent's report is kept until it is taken, and none of them is evicted.
func TestNoticesAreKeptForEveryRunningAgent(t *testing.T) {
	const n = 40
	m := NewManager(Config{MaxRunning: n}, instantRunner)
	defer m.StopAll(time.Second)
	for i := 0; i < n; i++ {
		if _, err := m.Spawn(SpawnOptions{Label: fmt.Sprint("agent ", i), Background: true, Prompt: "go"}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "every report", func() bool { return m.Active() == 0 && m.NoticeCount() == n })
	if notices := m.TakeNotices(""); len(notices) != n {
		t.Fatalf("got %d notices, want %d", len(notices), n)
	}
}

// TestSlotIsFreeOnceTurnDoneCloses: a turn gives its running slot back before it is
// announced, so a spawn right after TurnDone never hits the limit, even while the
// finished turn's goroutine has not wound down yet (the hook holds it there).
func TestSlotIsFreeOnceTurnDoneCloses(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	hold := make(chan chan struct{}, 1)
	testHookTurnReturned = func() {
		select {
		case release := <-hold:
			<-release
		default:
		}
	}
	defer func() { testHookTurnReturned = nil }()
	m := NewManager(Config{MaxRunning: 1}, instantRunner)
	defer m.StopAll(time.Second)
	for i := range 20 {
		release := make(chan struct{})
		hold <- release
		a, err := m.Spawn(SpawnOptions{Label: "quick", Prompt: fmt.Sprint(i)})
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		<-a.TurnDone()
		// a's goroutine is held in the hook, or about to be: the slot is free anyway.
		b, err := m.Spawn(SpawnOptions{Label: "next", Prompt: fmt.Sprint(i)})
		close(release)
		if err != nil {
			t.Fatalf("spawn right after TurnDone %d: %v", i, err)
		}
		<-b.TurnDone()
	}
	if stragglers := m.StopAll(time.Second); len(stragglers) > 0 {
		t.Fatalf("still running: %v", stragglers)
	}
}
