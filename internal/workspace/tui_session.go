//go:build darwin || linux || freebsd || netbsd || openbsd

package workspace

import (
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// sessionGroups lists the process groups with a process in the session leader started.
// A background job of an interactive shell stays in it after the shell exits, though
// it no longer has the terminal. Without ps it lists none.
func sessionGroups(leader int) []int {
	out, err := exec.Command("ps", "-A", "-o", "pid=,pgid=").Output()
	if err != nil {
		return nil
	}
	var groups []int
	fields := strings.Fields(string(out))
	for i := 0; i+1 < len(fields); i += 2 {
		pid, err1 := strconv.Atoi(fields[i])
		pgid, err2 := strconv.Atoi(fields[i+1])
		if err1 != nil || err2 != nil || pgid <= 1 || slices.Contains(groups, pgid) {
			continue
		}
		if sid, err := unix.Getsid(pid); err == nil && sid == leader {
			groups = append(groups, pgid)
		}
	}
	return groups
}
