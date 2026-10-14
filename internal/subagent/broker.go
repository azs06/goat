package subagent

import (
	"context"
	"sync"
)

// ApprovalRequest is a subagent asking the user to run a command. Main answers it
// with Answer; the reply channel holds one value, so answering never blocks, even
// when the agent has stopped waiting.
type ApprovalRequest struct {
	AgentID, Label, Command, Workdir string

	// Ctx is the requesting turn's context. A request whose Ctx is done is skipped.
	Ctx   context.Context
	reply chan error
}

// Answer sends the decision: nil approves, an error declines with that error.
// Only the first answer counts.
func (r *ApprovalRequest) Answer(err error) {
	select {
	case r.reply <- err:
	default:
	}
}

// ApprovalBroker queues approval requests from subagents in arrival order. Subagents
// call Request from their own goroutines; only main serves them, with Next, at fixed
// points, because only main may read the terminal.
type ApprovalBroker struct {
	mu      sync.Mutex
	queue   []*ApprovalRequest
	pending chan struct{}
}

// NewApprovalBroker returns an empty broker.
func NewApprovalBroker() *ApprovalBroker {
	return &ApprovalBroker{pending: make(chan struct{}, 1)}
}

// Request queues req and waits for main's answer or for ctx to end. When ctx ends
// first the request is withdrawn and the context's cause is returned.
func (b *ApprovalBroker) Request(ctx context.Context, req *ApprovalRequest) error {
	req.Ctx = ctx
	req.reply = make(chan error, 1)
	b.mu.Lock()
	b.queue = append(b.queue, req)
	b.mu.Unlock()
	select {
	case b.pending <- struct{}{}:
	default:
	}
	select {
	case err := <-req.reply:
		return err
	case <-ctx.Done():
		b.withdraw(req)
		return context.Cause(ctx)
	}
}

func (b *ApprovalBroker) withdraw(req *ApprovalRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, queued := range b.queue {
		if queued == req {
			b.queue = append(b.queue[:i:i], b.queue[i+1:]...)
			return
		}
	}
}

// Next takes the oldest request still waiting, or returns nil when there is none.
// Requests whose context has ended are dropped on the way.
func (b *ApprovalBroker) Next() *ApprovalRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.queue) > 0 {
		req := b.queue[0]
		b.queue[0] = nil
		b.queue = b.queue[1:]
		if req.Ctx.Err() == nil {
			return req
		}
	}
	b.queue = nil
	return nil
}

// Pending receives a value after a request is queued. It holds at most one and can
// be stale, so a server checks Len or loops on Next.
func (b *ApprovalBroker) Pending() <-chan struct{} {
	return b.pending
}

// Len is how many requests are queued, counting any whose context has just ended.
func (b *ApprovalBroker) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.queue)
}

// DrainAll answers every queued request with err, for shutdown.
func (b *ApprovalBroker) DrainAll(err error) {
	b.mu.Lock()
	queue := b.queue
	b.queue = nil
	b.mu.Unlock()
	for _, req := range queue {
		req.Answer(err)
	}
}
