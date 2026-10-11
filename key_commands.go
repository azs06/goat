package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/terminal"
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
// first command that prints a key settles it. A providers.json entry's key_command fills
// its key the same way. The key is kept out of run_bash commands,
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
			if command == "" && provider.KeyCommands[variable] != "" {
				commandVar, command = name+"'s key_command", provider.KeyCommands[variable]
			}
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
	// auth.json's commands, for the providers nothing else has supplied. Their keys stay
	// in goat's memory.
	for _, name := range provider.Names {
		command := authCommands[name]
		if command == "" || provider.CredentialSource(name) != "" {
			continue
		}
		key, err := runKeyCommand(command)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s's key_command in auth.json %v", name, err))
			continue
		}
		provider.StoreKey(name, key, false)
		authOrigins[name] = "key_command in " + displayPath(authPath())
	}
	return warnings
}

// runKeyCommand runs command in the user's shell and returns the one line it prints.
// The command shares goat's terminal, so a password manager can prompt on it; only its
// standard output is captured. Piped input is not passed on: it may be the prompt for
// goat -p.
func runKeyCommand(command string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keyCommandTimeout)
	defer cancel()
	cmd := shellCommand(ctx, command)
	var stdout bytes.Buffer
	if terminal.StdinIsTerminal() {
		cmd.Stdin = os.Stdin
	}
	// Its stderr goes to the screen for prompts, but not into a log goat's stderr is
	// redirected to, where a helper that echoes the key would leave it.
	if info, err := os.Stderr.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		cmd.Stderr = os.Stderr
	}
	cmd.Stdout = &stdout
	// A helper the command leaves running must not hold goat's startup; what it prints
	// later is ignored. On a timeout only the shell is stopped, so a command that runs
	// others should exec the last one.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("gave no key within %s", keyCommandTimeout)
	}
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		err = nil
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
