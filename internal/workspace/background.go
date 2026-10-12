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

// background holds the groups run_bash commands left running, oldest first, and the
// groups of the commands running right now.
var background struct {
	mu      sync.Mutex
	groups  []BackgroundGroup
	running map[int]bool
}

// settleWindow is how long a command's group may take to empty after its shell exits
// before what is left counts as running in the background. Login profiles can start
// helpers that outlive the shell by a few milliseconds.
const settleWindow = 150 * time.Millisecond

// trackRunning and untrackRunning bracket a command's run, so goat can stop it if it is
// closed mid-command.
func trackRunning(pgid int) {
	background.mu.Lock()
	defer background.mu.Unlock()
	if background.running == nil {
		background.running = map[int]bool{}
	}
	background.running[pgid] = true
}

func untrackRunning(pgid int) {
	background.mu.Lock()
	defer background.mu.Unlock()
	delete(background.running, pgid)
}

// rememberIfAlive records the process group of a command whose shell has exited when
// something in it is still running, and reports whether it did.
func rememberIfAlive(pgid int, command, dir string) bool {
	deadline := time.Now().Add(settleWindow)
	for groupAlive(pgid) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
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

// pruneLocked forgets the groups that are gone. background.mu must be held.
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
// recorded, and still has, can be stopped.
func StopBackgroundGroup(pgid int) error {
	background.mu.Lock()
	pruneLocked()
	known := false
	for _, group := range background.groups {
		known = known || group.PGID == pgid
	}
	background.mu.Unlock()
	if !known {
		return errUnknownGroup
	}
	if left := stopGroups([]int{pgid}); len(left) > 0 {
		return errStillRunning(pgid)
	}
	forgetGroups([]int{pgid})
	return nil
}

// StopAllBackground stops every group run_bash left running and any command still
// running, as goat exits, all at once, and returns the background groups it stopped.
func StopAllBackground() []BackgroundGroup {
	background.mu.Lock()
	pruneLocked()
	groups := append([]BackgroundGroup(nil), background.groups...)
	pgids := make([]int, 0, len(groups)+len(background.running))
	for pgid := range background.running {
		pgids = append(pgids, pgid)
	}
	background.mu.Unlock()
	for _, group := range groups {
		pgids = append(pgids, group.PGID)
	}
	if len(pgids) == 0 {
		return nil
	}
	left := map[int]bool{}
	for _, pgid := range stopGroups(pgids) {
		left[pgid] = true
	}
	var stopped []BackgroundGroup
	var gone []int
	for _, group := range groups {
		if !left[group.PGID] {
			stopped = append(stopped, group)
			gone = append(gone, group.PGID)
		}
	}
	forgetGroups(gone)
	return stopped
}

func forgetGroups(pgids []int) {
	background.mu.Lock()
	defer background.mu.Unlock()
	drop := map[int]bool{}
	for _, pgid := range pgids {
		drop[pgid] = true
	}
	kept := background.groups[:0]
	for _, group := range background.groups {
		if !drop[group.PGID] {
			kept = append(kept, group)
		}
	}
	background.groups = kept
}
