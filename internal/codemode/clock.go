package codemode

import (
	"context"
	"sync"
	"time"
)

// clock is a run's time budget. It can be paused while the run waits on the user, so
// an approval prompt does not use up the script's time.
type clock struct {
	mu        sync.Mutex
	remaining time.Duration
	started   time.Time
	timer     *time.Timer
	paused    int
	fired     bool
	onExpire  func()
}

func startClock(budget time.Duration, onExpire func()) *clock {
	c := &clock{remaining: budget, onExpire: onExpire}
	c.mu.Lock()
	c.arm()
	c.mu.Unlock()
	return c
}

// arm starts the timer with the remaining budget; c.mu is held.
func (c *clock) arm() {
	c.started = time.Now()
	c.timer = time.AfterFunc(c.remaining, func() {
		c.mu.Lock()
		if c.paused > 0 || c.fired {
			c.mu.Unlock()
			return
		}
		c.fired = true
		c.mu.Unlock()
		c.onExpire()
	})
}

// pause stops the clock until the returned function is called. Pauses nest.
func (c *clock) pause() (resume func()) {
	c.mu.Lock()
	if c.paused == 0 && !c.fired {
		c.timer.Stop()
		c.remaining -= time.Since(c.started)
	}
	c.paused++
	c.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.paused--
			if c.paused == 0 && !c.fired {
				if c.remaining < 0 {
					c.remaining = 0
				}
				c.arm()
			}
		})
	}
}

func (c *clock) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fired = true
	c.timer.Stop()
}

type clockKey struct{}

// PauseClock stops the run-time budget of the run_code script that ctx belongs to, until
// the returned function is called. Host functions call it around anything that waits on
// the user. Outside a run it does nothing.
func PauseClock(ctx context.Context) (resume func()) {
	if c, ok := ctx.Value(clockKey{}).(*clock); ok {
		return c.pause()
	}
	return func() {}
}
