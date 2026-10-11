package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/workspace"
)

// keyCommandTimeout bounds a key command. Password managers may wait for a fingerprint
// or a passphrase, so it is generous.
var keyCommandTimeout = 30 * time.Second

// runKeyCommands fills a provider's credential from a command named by the credential
// variable plus _COMMAND, such as
// OPENAI_API_KEY_COMMAND="op read op://Private/OpenAI/credential", so the key need not
// sit in a file. A provider that already has a credential, from the shell or the global
// .env, runs none of its commands; otherwise its variables are tried in order and the
// first command that prints a key settles it. The key is kept out of run_bash commands,
// like one from the global .env. It returns a warning for each command that failed;
// warnings never quote the command's output, which may be part of a key.
func runKeyCommands() (warnings []string) {
	for _, name := range provider.Names {
		if provider.CredentialSource(name) != "" {
			continue
		}
		for _, variable := range provider.CredentialVars[name] {
			commandVar := variable + "_COMMAND"
			command := strings.TrimSpace(os.Getenv(commandVar))
			if command == "" {
				continue
			}
			key, err := runKeyCommand(command)
			if err == nil {
				err = os.Setenv(variable, key)
			}
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s %v", commandVar, err))
				continue
			}
			envSources[variable] = commandVar
			workspace.HideFromCommands(variable)
			break
		}
	}
	return warnings
}

// runKeyCommand runs command in the user's shell and returns the one line it prints.
// The command shares goat's terminal, so a password manager can prompt on it; only its
// standard output is captured.
func runKeyCommand(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keyCommandTimeout)
	defer cancel()
	cmd := shellCommand(ctx, command)
	var stdout bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, &stdout, os.Stderr
	// A helper the command leaves running must not hold goat's startup.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("gave no key within %s", keyCommandTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("failed: %v", err)
	}
	key := strings.TrimSpace(stdout.String())
	if key == "" {
		return "", errors.New("printed nothing; it should print the key")
	}
	if strings.ContainsAny(key, "\r\n") {
		return "", errors.New("printed more than one line; it should print only the key")
	}
	return key, nil
}
