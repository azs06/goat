package main

import (
	"strings"

	"github.com/azs06/goat/internal/provider"
)

// dotenvKeys are the variables goat loaded from .env at startup. They are secrets the
// user gave goat, not the shell, so commands the model runs do not inherit them.
var dotenvKeys = map[string]bool{}

// isCredentialVar reports environment variables that hold API credentials: the ones
// goat's providers use, and anything named like a key, token or secret.
func isCredentialVar(name string) bool {
	upper := strings.ToUpper(name)
	for _, vars := range provider.CredentialVars {
		for _, credential := range vars {
			if upper == credential {
				return true
			}
		}
	}
	for _, suffix := range []string{"_API_KEY", "_APIKEY", "_SECRET", "_SECRET_KEY", "_ACCESS_KEY", "_AUTH_TOKEN", "_PASSWORD"} {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}

// commandEnvironment is the environment run_bash commands get: goat's own, without
// credentials or anything loaded from .env. A command that prints its environment
// would otherwise put the user's keys in the transcript the model provider receives.
func commandEnvironment(environ []string) []string {
	kept := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if dotenvKeys[name] || isCredentialVar(name) {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

// mentionsStateDir reports a command that names goat's state directory. Such commands
// are always confirmed, even when commands run without asking, since run_bash changes
// are not snapshotted and .goat holds the history and undo snapshots.
func mentionsStateDir(command string) bool {
	return strings.Contains(strings.ToLower(command), strings.ToLower(stateDirName))
}
