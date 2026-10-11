package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/azs06/goat/internal/style"
)

// themePath is the theme file goat uses: the one GOAT_THEME names, or theme.json in
// goat's config directory ($XDG_CONFIG_HOME/goat, or ~/.config/goat). explicit reports
// that GOAT_THEME named it, so a missing file is worth a warning.
func themePath() (path string, explicit bool) {
	if path := strings.TrimSpace(os.Getenv("GOAT_THEME")); path != "" {
		return path, true
	}
	dir := goatConfigDir()
	if dir == "" {
		return "", false
	}
	return filepath.Join(dir, "theme.json"), false
}

// applyTheme makes the configured theme current: the built-in theme GOAT_THEME names,
// or else the theme file (GOAT_THEME's path, or theme.json in the config directory)
// layered over the built-in default or the theme its "extends" names. Declarations that
// cannot be used are warned about and skipped; a file that cannot be read or is not
// valid JSON leaves the default theme in place. It returns what is in use, such as
// "the built-in one-dark theme" or the file's path, or "" for the default theme.
func applyTheme() string {
	if name, ok := style.BuiltinName(os.Getenv("GOAT_THEME")); ok {
		theme, _ := style.Builtin(name)
		style.Use(theme)
		return "the built-in " + name + " theme"
	}
	path, explicit := themePath()
	if path == "" {
		style.Use(style.Default())
		return ""
	}
	theme, problems, err := style.LoadTheme(path)
	if err != nil {
		style.Use(style.Default())
		if explicit || !errors.Is(err, fs.ErrNotExist) {
			warnf("theme %s not used: %v", path, err)
		}
		return ""
	}
	for _, problem := range problems {
		warnf("theme %s: %s", path, problem)
	}
	style.Use(theme)
	return path
}

// commandTheme switches theme and saves the choice in config.json: /theme <name> for a
// built-in theme, or /theme alone to pick one with the arrow keys. Without a terminal
// to pick on, /theme reloads the configured theme and lists the built-in ones.
func commandTheme(ctx context.Context, c *config, args []string) error {
	if len(args) > 0 {
		// "/theme one dark" names one theme.
		wanted := strings.Join(args, " ")
		name, ok := style.BuiltinName(wanted)
		if !ok {
			if suggestion, ok := style.SuggestBuiltin(wanted); ok {
				return fmt.Errorf("there is no built-in theme %q; did you mean %s? The built-in themes are %s", wanted, suggestion, strings.Join(style.BuiltinNames(), ", "))
			}
			return fmt.Errorf("there is no built-in theme %q; the built-in themes are %s", wanted, strings.Join(style.BuiltinNames(), ", "))
		}
		return switchTheme(name)
	}

	options, values := themeOptions()
	chosen, err := selectOptionInteractive(ctx, c, "Theme", options)
	switch {
	case errors.Is(err, errSelectorUnavailable):
		return reloadTheme()
	case err != nil:
		return err
	case chosen < 0:
		fmt.Println(formatNotice("Keeping the " + themeLabel(currentThemeSetting()) + "."))
		return nil
	default:
		return switchTheme(values[chosen])
	}
}

// themeOptions lists the built-in themes, then the theme file when there is one, with
// the theme in use marked. values holds what each option sets "theme" to.
func themeOptions() (options []selectOption, values []string) {
	current := currentThemeSetting()
	currentName, currentIsBuiltin := style.BuiltinName(current)
	for _, name := range style.BuiltinNames() {
		options = append(options, selectOption{label: name, hint: themeHints[name], current: currentIsBuiltin && name == currentName})
		values = append(values, name)
	}
	if path := themeFileCandidate(); path != "" {
		options = append(options, selectOption{label: displayPath(path), hint: "your theme file", current: !currentIsBuiltin})
		values = append(values, path)
	}
	return options, values
}

// themeFileCandidate is the theme file /theme offers: the one GOAT_THEME names, or else
// theme.json in the config directory, also while a built-in theme is chosen. "" when
// there is none.
func themeFileCandidate() string {
	path := strings.TrimSpace(os.Getenv("GOAT_THEME"))
	if _, builtin := style.BuiltinName(path); builtin || path == "" {
		if dir := goatConfigDir(); dir != "" {
			path = filepath.Join(dir, "theme.json")
		}
	}
	if info, err := os.Stat(path); path == "" || err != nil || info.IsDir() {
		return ""
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return path
}

// currentThemeSetting is the theme in use: a built-in theme's name or the theme file's
// path, as config.json's "theme" would say it. The default theme is "default".
func currentThemeSetting() string {
	if name, ok := style.BuiltinName(os.Getenv("GOAT_THEME")); ok {
		return name
	}
	if path := themeFileCandidate(); path != "" {
		return path
	}
	return style.DefaultThemeName
}

// themeLabel names a theme setting for messages: "one-dark theme" or "theme in
// ~/.config/goat/theme.json".
func themeLabel(setting string) string {
	if name, ok := style.BuiltinName(setting); ok {
		return name + " theme"
	}
	return "theme in " + displayPath(setting)
}

// switchTheme makes setting, a built-in theme's name or a theme file's path, the theme
// now and at the next start: it is saved as "theme" in config.json. A theme file that
// cannot be used changes nothing.
func switchTheme(setting string) error {
	theme, ok := style.Builtin(setting)
	if name, builtin := style.BuiltinName(setting); builtin {
		setting = name
	} else {
		// config.json takes only full paths.
		if absolute, err := filepath.Abs(setting); err == nil {
			setting = absolute
		}
		loaded, problems, err := style.LoadTheme(setting)
		if err != nil {
			return fmt.Errorf("the theme in %s cannot be used: %w", displayPath(setting), err)
		}
		for _, problem := range problems {
			warnf("theme %s: %s", setting, problem)
		}
		theme, ok = loaded, true
	}
	if !ok {
		return fmt.Errorf("there is no theme %q", setting)
	}
	style.Use(theme)

	// The shell's GOAT_THEME wins over config.json at the next start, so it is left
	// alone and only this session changes. Once goat has set it, it is goat's.
	message := "Switched to the " + themeLabel(setting) + "."
	if strings.TrimSpace(os.Getenv("GOAT_THEME")) != "" && envSources["GOAT_THEME"] == "" {
		fmt.Println(formatSuccess(message) + style.Muted.Render(" GOAT_THEME is set in your shell, so it applies again at the next start."))
		return nil
	}
	os.Setenv("GOAT_THEME", setting)
	if configPath() == "" {
		envSources["GOAT_THEME"] = "/theme"
		fmt.Println(formatSuccess(message) + style.Muted.Render(" There is no config directory to save it in, so it lasts for this session."))
		return nil
	}
	envSources["GOAT_THEME"] = displayPath(configPath())
	value, _ := json.Marshal(setting)
	if err := writeJSONField(configPath(), "config.json", "theme", value, configFileMode()); err != nil {
		return fmt.Errorf("switched for this session, but saving it failed: %w", err)
	}
	fmt.Println(formatSuccess(message) + style.Muted.Render(" Saved in "+displayPath(configPath())+"."))
	return nil
}

// configFileMode keeps config.json's permissions when it exists.
func configFileMode() os.FileMode {
	if info, err := os.Stat(configPath()); err == nil {
		return info.Mode().Perm()
	}
	return 0o644
}

// reloadTheme reads the configured theme again, such as after editing the theme file,
// and lists the built-in themes.
func reloadTheme() error {
	using := applyTheme()
	switch {
	case using == "":
		wanted, _ := themePath()
		fmt.Println("Using the default theme. " + style.Muted.Render(fmt.Sprintf(`Set "theme" in %s, or put your own in %s.`, displayPath(configPath()), wanted)))
	case strings.HasPrefix(using, "the built-in "):
		fmt.Println("Using " + using + ".")
	default:
		fmt.Println(formatSuccess("Reloaded the theme from " + using))
	}
	fmt.Println(style.Muted.Render("Built-in themes: " + strings.Join(style.BuiltinNames(), ", ") + ". /theme <name> switches."))
	return nil
}
