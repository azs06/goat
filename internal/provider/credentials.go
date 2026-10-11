package provider

import (
	"os"
	"slices"
	"strings"
)

// StoredSource is the source CredentialSource reports for a key goat holds in memory,
// read from auth.json or printed by its key_command. Such keys never go into the
// environment, so no command goat runs can inherit them.
const StoredSource = "auth.json"

// storedCredential is a key from auth.json. authToken marks an Anthropic auth token,
// which is sent as a bearer token instead of an API key.
type storedCredential struct {
	value     string
	authToken bool
}

var stored = map[string]storedCredential{}

// namedInAuth are the providers auth.json supplies. They need a key even when nothing
// else says so, as for an entry with no key_env, and say so when a key_command fails.
var namedInAuth = map[string]bool{}

// NameInAuth records that auth.json supplies provider.
func NameInAuth(provider string) { namedInAuth[provider] = true }

// NamedInAuth reports a provider auth.json supplies.
func NamedInAuth(provider string) bool { return namedInAuth[provider] }

// StoreKey keeps a provider's key from auth.json in memory.
func StoreKey(provider, value string, authToken bool) {
	stored[provider] = storedCredential{value: strings.TrimSpace(value), authToken: authToken}
}

// ClearStoredKeys forgets every stored key, for tests.
func ClearStoredKeys() {
	stored = map[string]storedCredential{}
	namedInAuth = map[string]bool{}
}

// Credential returns the provider's credential and where it comes from. authToken marks
// an Anthropic auth token (ANTHROPIC_AUTH_TOKEN or auth_token). source is "" when there
// is none. The order is:
//
//   - its key from auth.json, which goat only stores when the shell sets none of the
//     provider's variables, so the shell has already won;
//   - for an entry whose key_env names a built-in key, such as OPENAI_API_KEY, that
//     built-in's key from auth.json, since that is where the key now lives;
//   - its first variable that is set, from the shell or the global .env.
func Credential(provider string) (source, value string, authToken bool) {
	if credential, ok := stored[provider]; ok && credential.value != "" {
		return StoredSource, credential.value, credential.authToken
	}
	for _, name := range CredentialVars[provider] {
		for _, owner := range builtIns {
			credential, ok := stored[owner]
			if owner == provider || !ok || credential.value == "" || !slices.Contains(CredentialVars[owner], name) {
				continue
			}
			// ANTHROPIC_API_KEY names an API key and ANTHROPIC_AUTH_TOKEN a token.
			if name == "ANTHROPIC_AUTH_TOKEN" && !credential.authToken || name == "ANTHROPIC_API_KEY" && credential.authToken {
				continue
			}
			return StoredSource, credential.value, credential.authToken
		}
	}
	for _, name := range CredentialVars[provider] {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return name, value, name == "ANTHROPIC_AUTH_TOKEN"
		}
	}
	return "", "", false
}
