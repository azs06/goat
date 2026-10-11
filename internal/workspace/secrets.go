package workspace

import (
	"runtime"
	"strings"

	"github.com/azs06/goat/internal/provider"
)

// dotenvKeys are the variables goat loaded from its global .env or a key command at
// startup, by EnvName. They are secrets the user gave goat, not the shell, so commands
// the model runs do not inherit them.
var dotenvKeys = map[string]bool{}

// CaseInsensitiveEnv is set where environment variable names ignore case, as on
// Windows: there xdg_config_home and XDG_CONFIG_HOME are one variable.
var CaseInsensitiveEnv = runtime.GOOS == "windows"

// EnvName is the name to compare an environment variable by: upper case where names
// ignore case, as written elsewhere.
func EnvName(name string) string {
	if CaseInsensitiveEnv {
		return strings.ToUpper(name)
	}
	return name
}

// HideFromCommands keeps the named variables out of the environment run_bash commands
// get. Call it at startup with the variables goat set itself.
func HideFromCommands(names ...string) {
	for _, name := range names {
		dotenvKeys[EnvName(name)] = true
	}
}

// isCredentialVar reports environment variables that hold API credentials: the ones
// goat's providers use, and anything named like a key, token or secret.
func isCredentialVar(name string) bool {
	upper := strings.ToUpper(name)
	// A command that prints a key, such as OPENAI_API_KEY_COMMAND, is the recipe for it.
	if base, ok := strings.CutSuffix(upper, "_COMMAND"); ok && isCredentialVar(base) {
		return true
	}
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

// isEndpointVar reports a provider's base URL variable, such as OPENAI_BASE_URL. Some
// proxies take a token in the URL, so it is kept from commands like a key.
func isEndpointVar(name string) bool {
	upper := strings.ToUpper(name)
	for _, variable := range provider.BaseURLVars {
		if upper == variable {
			return true
		}
	}
	return false
}

// CommandEnvironment is the environment run_bash commands get: goat's own, without
// credentials or anything goat loaded itself. A command that prints its environment would
// otherwise put the user's keys in the transcript the model provider receives.
func CommandEnvironment(environ []string) []string {
	kept := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if dotenvKeys[EnvName(name)] || isCredentialVar(name) || isEndpointVar(name) {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

// MentionsStateDir reports a command that names goat's state directory. Such commands
// are always confirmed, even when commands run without asking, since run_bash changes
// are not snapshotted and .goat holds the history and undo snapshots.
func MentionsStateDir(command string) bool {
	return strings.Contains(strings.ToLower(command), strings.ToLower(StateDirName))
}
