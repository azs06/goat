package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/azs06/goat/internal/jsonfile"
	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/terminal"
)

// /login saves a provider's API key to auth.json and starts using it at once. For the
// few providers that let people sign in with an account instead, that choice is listed
// but not available yet.

// keylessNotice is shown when goat starts with no provider it can use.
func keylessNotice() string {
	return "No provider has a key yet. Type /login to add one."
}

// loginInstructions say how to add a key without /login, which needs a terminal.
func loginInstructions() string {
	return fmt.Sprintf(`/login needs an interactive terminal. Instead, add the key to %s, such as {"openai": {"key": "sk-..."}}, and start goat again.`, displayPath(authPath()))
}

func commandLogin(ctx context.Context, c *config, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: /login [provider]")
	}
	if c.input == nil || !c.interactive {
		fmt.Println(loginInstructions())
		return nil
	}
	path := authPath()
	if path == "" {
		return errors.New("goat has no config directory to keep auth.json in; set HOME or XDG_CONFIG_HOME")
	}

	var name string
	if len(args) == 1 {
		name = strings.ToLower(strings.TrimSpace(args[0]))
		if !isProviderName(name) {
			return fmt.Errorf("there is no provider named %s; the providers are %s", args[0], strings.Join(provider.Names, ", "))
		}
	} else {
		picked, err := pickLoginProvider(ctx, c)
		if err != nil || picked == "" {
			if err == nil {
				fmt.Println(formatNotice("Nothing changed."))
			}
			return err
		}
		name = picked
	}
	label := provider.Label(name)
	// Where the key will go, before it is typed: a keyless start printed no notices.
	if notice := endpointNotice(name); notice != "" {
		fmt.Println(formatWarning(notice))
	}

	if ok, err := pickLoginMethod(ctx, c, name); err != nil || !ok {
		if err == nil {
			fmt.Println(formatNotice("Nothing changed."))
		}
		return err
	}

	previous, err := authEntryKind(path, name)
	if err != nil {
		return err
	}
	if previous == "key_command" || previous == "auth_token" {
		answer, err := c.ask(ctx, fmt.Sprintf("%s has a %s in %s; replace it with an API key? [y/N] ", label, previous, displayPath(path)))
		if err != nil {
			return err
		}
		if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			fmt.Println(formatNotice("Nothing changed."))
			return nil
		}
	}

	key, err := readSecret(ctx, c, label+" API key (hidden; Enter saves, Esc cancels): ")
	if errors.Is(err, errSecretCancelled) || err == nil && key == "" {
		fmt.Println(formatNotice("Nothing saved."))
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkAPIKey(key); err != nil {
		return err
	}

	if err := writeAuthKey(path, name, key); err != nil {
		return err
	}
	fmt.Println(formatSuccess(fmt.Sprintf("Saved the %s key to %s, which only you can read.", label, displayPath(path))))
	if variable := applyLogin(c, name, key); variable != "" {
		fmt.Println(formatWarning(fmt.Sprintf("%s is set in your shell, and the shell wins over auth.json; unset it to use the saved key.", variable)))
		return nil
	}
	return checkLogin(ctx, c, name, key)
}

func isProviderName(name string) bool {
	for _, known := range provider.Names {
		if known == name {
			return true
		}
	}
	return false
}

// loginGroup is the heading a provider is listed under in /login.
func loginGroup(name string) string {
	entry, isEntry := provider.LookupEntry(name)
	switch {
	case !isEntry:
		return "Providers"
	case entry.Preset:
		return "More providers"
	default:
		return "Your providers (" + filepath.Base(entry.Source) + ")"
	}
}

// loginStatus says whether a provider has a key now, for /login's list.
func loginStatus(name string) string {
	if origin := credentialOrigin(name); origin != "" {
		return origin
	}
	if entry, ok := provider.LookupEntry(name); ok && entry.Keyless() && provider.Available(name) {
		return "needs no key"
	}
	return "no key"
}

// pickLoginProvider asks which provider to save a key for. It returns "" when the user
// cancels.
func pickLoginProvider(ctx context.Context, c *config) (string, error) {
	names := provider.Names
	options := make([]selectOption, 0, len(names))
	for _, name := range names {
		hint := loginStatus(name)
		if label := provider.Label(name); label != name {
			hint = name + " · " + hint
		}
		options = append(options, selectOption{label: provider.Label(name), hint: hint, group: loginGroup(name), current: name == c.model.Provider})
	}
	chosen, err := selectOptionInteractive(ctx, c, "Log in to a provider", options)
	if !errors.Is(err, errSelectorUnavailable) {
		if err != nil || chosen < 0 {
			return "", err
		}
		return names[chosen], nil
	}

	fmt.Println(style.Strong.Render("Providers"))
	for i, name := range names {
		fmt.Printf("  %2d. %s %s\n", i+1, provider.Label(name), style.Muted.Render("("+name+", "+loginStatus(name)+")"))
	}
	answer, err := c.ask(ctx, "Log in to which provider? (number or name, Enter cancels): ")
	if err != nil || answer == "" {
		return "", nil
	}
	if number, err := strconv.Atoi(answer); err == nil {
		if number < 1 || number > len(names) {
			return "", fmt.Errorf("pick a number from 1 to %d", len(names))
		}
		return names[number-1], nil
	}
	if name := strings.ToLower(answer); isProviderName(name) {
		return name, nil
	}
	return "", fmt.Errorf("there is no provider named %s", answer)
}

// pickLoginMethod asks how to sign in, for a provider that has an account to sign in
// with; the others take only an API key, so nothing is asked. Only an API key works for
// now. ok is false when the user cancels.
func pickLoginMethod(ctx context.Context, c *config, name string) (bool, error) {
	account := provider.SignIn(name)
	if account == "" {
		return true, nil
	}
	label := provider.Label(name)
	options := []selectOption{
		{label: "API key", hint: "paste a key from " + label + "'s console"},
		{label: "Sign in with your " + account, hint: "not available yet", disabled: true},
	}
	chosen, err := selectOptionInteractive(ctx, c, "Sign in to "+label+" with", options)
	if errors.Is(err, errSelectorUnavailable) {
		fmt.Println(style.Muted.Render("Signing in with an API key; signing in with your " + account + " is not available yet."))
		return true, nil
	}
	return chosen == 0, err
}

// checkAPIKey refuses what cannot be a key, without repeating it.
func checkAPIKey(key string) error {
	if strings.ContainsFunc(key, unicode.IsSpace) {
		return errors.New("that is not an API key: it has spaces in it; nothing was saved")
	}
	return nil
}

var (
	errSecretCancelled = errors.New("cancelled")
	// errSecretLines is a paste of more than one line. The lines after the first would
	// otherwise be left for the prompt, and sent to the model.
	errSecretLines = errors.New("the pasted text has more than one line; paste just the key. Nothing was saved")
)

// secretDrainWait is how long readSecret waits for the rest of a paste after Enter.
var secretDrainWait = 50 * time.Millisecond

// readSecret reads a line without showing it, such as an API key. It needs a terminal
// whose echo goat can turn off; elsewhere it refuses rather than show the key.
func readSecret(ctx context.Context, c *config, prompt string) (string, error) {
	resume := suspendInputGuard()
	defer resume()
	restore, err := terminal.EnterCbreak()
	if err != nil {
		return "", fmt.Errorf("goat cannot hide what you type in this terminal, so it will not ask for the key; add it to %s instead", displayPath(authPath()))
	}
	defer restore()

	// The prompt gets its own line and only the short count below it is redrawn, so a
	// prompt wider than the terminal never wraps into the redraw. Plain output gets no
	// redraws at all.
	fmt.Println(prompt)
	show := func(count int) {
		if style.Enabled() {
			fmt.Print("\r\x1b[K" + style.Muted.Render(fmt.Sprintf("  [%d characters]", count)))
		}
	}
	show(0)
	secret, err := collectSecret(ctx, c.input.readKey, show)
	if err == nil && drainInput(ctx, c.input.readKey, secretDrainWait) {
		// More lines came right behind the first: a paste without bracketed paste.
		err = errSecretLines
	}
	fmt.Println()
	if err != nil {
		return "", err
	}
	return secret, nil
}

// drainInput discards keys that arrive within wait of each other, as the rest of a
// paste does, and reports whether there were any.
func drainInput(ctx context.Context, readKey func(context.Context) (string, error), wait time.Duration) bool {
	drained := false
	for {
		waitCtx, cancel := context.WithTimeout(ctx, wait)
		_, err := readKey(waitCtx)
		cancel()
		if err != nil {
			return drained
		}
		drained = true
	}
}

// collectSecret gathers keys up to Enter. Backspace and Ctrl-U edit; Esc and Ctrl-C
// cancel; escape sequences, such as arrows, and other control keys are ignored. A
// bracketed paste with a line break in it is read to its end and refused. shown is told
// how many characters there are after each change, never what they are.
func collectSecret(ctx context.Context, readKey func(context.Context) (string, error), shown func(count int)) (string, error) {
	var secret []rune
	pasting, lines := false, false
	for {
		key, err := readKey(ctx)
		if err != nil {
			return "", err
		}
		switch {
		case key == pasteStart:
			pasting = true
			continue
		case key == pasteEnd:
			pasting = false
			if lines {
				return "", errSecretLines
			}
			continue
		case pasting && (key == "\r" || key == "\n"):
			lines = true
			continue
		case lines:
			continue
		case key == "\r" || key == "\n":
			return strings.TrimSpace(string(secret)), nil
		case key == "\x1b" || key == "\x03" || key == "\x04":
			return "", errSecretCancelled
		case key == "\x7f" || key == "\x08":
			if len(secret) > 0 {
				secret = secret[:len(secret)-1]
			}
		case key == "\x15":
			secret = nil
		case strings.HasPrefix(key, "\x1b"):
			continue
		default:
			runes := []rune(key)
			if len(runes) != 1 || unicode.IsControl(runes[0]) {
				continue
			}
			secret = append(secret, runes[0])
		}
		shown(len(secret))
	}
}

// authEntryKind is what auth.json holds for a provider now: "key", "auth_token",
// "key_command", or "" when it has nothing for it or there is no file.
func authEntryKind(path, name string) (string, error) {
	fields, err := readAuthFields(path)
	if err != nil {
		return "", err
	}
	for _, field := range fields {
		if field.Name != name {
			continue
		}
		var members map[string]json.RawMessage
		if json.Unmarshal(field.Value, &members) != nil {
			return "key", nil
		}
		for _, kind := range authFields {
			if _, ok := members[kind]; ok {
				return kind, nil
			}
		}
	}
	return "", nil
}

// readAuthFields reads auth.json's top-level fields, in order. A missing or empty file
// has none. Errors never quote the file.
func readAuthFields(path string) ([]jsonfile.Field, error) {
	return readJSONFields(path, "auth.json")
}

// readJSONFields reads the top-level fields of a JSON object file, in order. A missing
// or empty file has none. Errors never quote the file, which may hold keys.
func readJSONFields(path, what string) ([]jsonfile.Field, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	fields, err := jsonfile.Object(data, what+" must be a JSON object")
	if err != nil {
		return nil, fmt.Errorf("%s: %w; fix it, then try again", displayPath(path), err)
	}
	return fields, nil
}

// writeAuthKey sets a provider's key in auth.json and keeps everything else in it. The
// file is readable only by the user.
func writeAuthKey(path, name, key string) error {
	var value bytes.Buffer
	encoder := json.NewEncoder(&value)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(map[string]string{"key": key}); err != nil {
		return err
	}
	return writeJSONField(path, "auth.json", name, bytes.TrimSpace(value.Bytes()), 0o600)
}

// writeJSONField sets one top-level field of a JSON object file (auth.json, config.json)
// and keeps the others, in order. The file is replaced in one step with mode perm; a
// symbolic link, as from a dotfiles repository, is followed and its target replaced.
func writeJSONField(path, what, name string, value json.RawMessage, perm os.FileMode) error {
	path, err := followLinks(path)
	if err != nil {
		return err
	}
	fields, err := readJSONFields(path, what)
	if err != nil {
		return err
	}

	entry := jsonfile.Field{Name: name, Value: value}
	replaced := false
	for i := range fields {
		if fields[i].Name == name {
			fields[i], replaced = entry, true
		}
	}
	if !replaced {
		fields = append(fields, entry)
	}

	var out bytes.Buffer
	out.WriteString("{\n")
	for i, field := range fields {
		quoted, err := json.Marshal(field.Name)
		if err != nil {
			return err
		}
		var indented bytes.Buffer
		if err := json.Indent(&indented, field.Value, "  ", "  "); err != nil {
			return err
		}
		out.WriteString("  " + string(quoted) + ": " + indented.String())
		if i < len(fields)-1 {
			out.WriteString(",")
		}
		out.WriteString("\n")
	}
	out.WriteString("}\n")

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+strings.TrimSuffix(filepath.Base(path), ".json")+"-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(out.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// followLinks resolves path through symbolic links, including one whose target does not
// exist yet, as a dotfiles repository may leave for a file kept out of it.
func followLinks(path string) (string, error) {
	for range 40 {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			// A missing file, or a real one, possibly in a linked directory.
			if resolved, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
				return filepath.Join(resolved, filepath.Base(path)), nil
			}
			return path, nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return "", fmt.Errorf("%s: too many symbolic links", displayPath(path))
}

// shellCredential is the provider's credential variable the shell sets, directly or as
// a _COMMAND, which keeps auth.json from being used for it; "" when there is none. The
// files goat loaded record the variables they set, so theirs are not the shell's.
func shellCredential(name string) string {
	for _, variable := range provider.CredentialVars[name] {
		if strings.TrimSpace(os.Getenv(variable)) != "" && envSources[variable] == "" {
			return variable
		}
		command := variable + "_COMMAND"
		if strings.TrimSpace(os.Getenv(command)) != "" && envSources[command] == "" {
			return command
		}
	}
	return ""
}

// applyLogin starts using a key /login saved, as auth.json would at the next start. It
// returns the shell variable that wins over the key instead, if there is one.
func applyLogin(c *config, name, key string) string {
	if variable := shellCredential(name); variable != "" {
		return variable
	}
	provider.StoreKey(name, key, false)
	provider.NameInAuth(name)
	authSettled[name] = true
	authOrigins[name] = displayPath(authPath())
	delete(authCommands, name)
	// Clients made with the old key, including entries that share it, are made again.
	c.forgetProviders(func(cached string) bool {
		return cached == name || sharesCredential(cached, name)
	})
	return ""
}

func sharesCredential(a, b string) bool {
	for _, x := range provider.CredentialVars[a] {
		for _, y := range provider.CredentialVars[b] {
			if x == y {
				return true
			}
		}
	}
	return false
}

// checkLogin asks the provider for its models with the new key, says whether that
// worked, and picks a model when goat has none.
func checkLogin(ctx context.Context, c *config, name, key string) error {
	p, err := c.provider(name)
	if err != nil {
		fmt.Println(formatWarning(err.Error()))
		return nil
	}
	startSpinner("Checking the key")
	listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	models, err := p.ListModels(listCtx)
	cancel()
	stopSpinner()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		// Some errors repeat the key; it is cut out before the message is shortened, which
		// could leave a piece of it.
		message := shortError(errors.New(strings.ReplaceAll(err.Error(), key, "[key]")))
		fmt.Println(formatWarning(fmt.Sprintf("%s did not accept the key when goat listed its models: %s. Run /login again to replace it.", provider.Label(name), message)))
	} else {
		fmt.Println(formatNotice(fmt.Sprintf("The key works: %s lists %d models.", provider.Label(name), len(models))))
	}
	if !c.model.IsZero() {
		return nil
	}

	if model := provider.DefaultModel(); !model.IsZero() {
		return switchModel(ctx, c, model.String())
	}
	if len(models) > 0 {
		picked, err := pickModelInteractive(ctx, c, provider.ModelChoices(c.model, map[string][]string{name: models}), false)
		if !errors.Is(err, errSelectorUnavailable) {
			if err != nil || picked.IsZero() {
				return err
			}
			return switchModel(ctx, c, picked.String())
		}
	}
	fmt.Println(formatNotice("Pick a model with /model."))
	return nil
}
