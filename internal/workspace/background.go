package workspace

import (
	"errors"
	"sync"
	"time"
)

var errUnknownGroup = errors.New("goat did not start that process group")

// BackgroundGroup is a command whose shell has exited while processes it started, such
// as a dev server launched with "&", keep running in its process group.
type BackgroundGroup struct {
	PGID    int
	Command string
	// Dir is the working directory, relative to the workspace.
	Dir     string
	Started time.Time
}

// GroupProcess is one process still running in a background group.
type GroupProcess struct {
	PID int
	// Elapsed is how long it has run, as ps prints it, such as "03:12".
	Elapsed string
	Command string
}

// background holds the groups run_bash commands left running, oldest first.
var background struct {
	mu     sync.Mutex
	groups []BackgroundGroup
}

// rememberIfAlive records the process group of a command whose shell has exited when
// something in it is still running, and reports whether it did.
func rememberIfAlive(pgid int, command, dir string) bool {
	if !groupAlive(pgid) {
		return false
	}
	background.mu.Lock()
	defer background.mu.Unlock()
	pruneLocked()
	background.groups = append(background.groups, BackgroundGroup{PGID: pgid, Command: command, Dir: dir, Started: time.Now()})
	return true
}

// BackgroundGroups lists the groups still running, oldest first. Groups whose
// processes have all exited are forgotten.
func BackgroundGroups() []BackgroundGroup {
	background.mu.Lock()
	defer background.mu.Unlock()
	pruneLocked()
	return append([]BackgroundGroup(nil), background.groups...)
}

// pruneLocked forgets the groups with nothing left running, before their process
// group IDs can be reused. background.mu must be held.
func pruneLocked() {
	alive := background.groups[:0]
	for _, group := range background.groups {
		if groupAlive(group.PGID) {
			alive = append(alive, group)
		}
	}
	background.groups = alive
}

// StopBackgroundGroup stops every process in a group run_bash left running: SIGTERM
// first, then SIGKILL for whatever is left a few seconds later. Only groups goat
// recorded can be stopped, so a reused process ID is never signalled.
func StopBackgroundGroup(pgid int) error {
	background.mu.Lock()
	known := false
	for _, group := range background.groups {
		known = known || group.PGID == pgid
	}
	background.mu.Unlock()
	if !known {
		return errUnknownGroup
	}
	err := stopGroup(pgid)
	if err == nil {
		forgetGroup(pgid)
	}
	return err
}

// StopAllBackground stops every group run_bash left running, as goat exits, and
// returns the ones it stopped.
func StopAllBackground() []BackgroundGroup {
	var stopped []BackgroundGroup
	for _, group := range BackgroundGroups() {
		if StopBackgroundGroup(group.PGID) == nil {
			stopped = append(stopped, group)
		}
	}
	return stopped
}

func forgetGroup(pgid int) {
	background.mu.Lock()
	defer background.mu.Unlock()
	kept := background.groups[:0]
	for _, group := range background.groups {
		if group.PGID != pgid {
			kept = append(kept, group)
		}
	}
	background.groups = kept
}
