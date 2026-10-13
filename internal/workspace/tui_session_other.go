//go:build unix && !(darwin || linux || freebsd || netbsd || openbsd)

package workspace

// sessionGroups would list the process groups in leader's session; this system has no
// getsid for goat to find them with, so run_tui ends the groups on the terminal only.
func sessionGroups(leader int) []int { return nil }
