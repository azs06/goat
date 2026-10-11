package provider

import (
	"os"
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

// StoreKey keeps a provider's key from auth.json in memory.
func StoreKey(provider, value string, authToken bool) {
	stored[provider] = storedCredential{value: strings.TrimSpace(value), authToken: authToken}
}

// ClearStoredKeys forgets every stored key, for tests.
func ClearStoredKeys() {
	stored = map[string]storedCredential{}
}

// Credential returns the provider's credential and where it comes from: the first of its
// variables that is set, or else a key from auth.json. authToken marks an Anthropic
// auth token (ANTHROPIC_AUTH_TOKEN or auth_token). source is "" when there is none.
func Credential(provider string) (source, value string, authToken bool) {
	for _, name := range CredentialVars[provider] {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return name, value, name == "ANTHROPIC_AUTH_TOKEN"
		}
	}
	if credential, ok := stored[provider]; ok && credential.value != "" {
		return StoredSource, credential.value, credential.authToken
	}
	return "", "", false
}
