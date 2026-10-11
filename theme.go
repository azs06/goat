package main

import (
	"context"
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

// commandTheme switches to a built-in theme for this session, or with no argument
// reloads the configured theme and lists the built-in ones.
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
		theme, _ := style.Builtin(name)
		style.Use(theme)
		fmt.Println(formatSuccess("Switched to the "+name+" theme for this session.") + style.Muted.Render(fmt.Sprintf(` To keep it, set "theme": %q in %s.`, name, displayPath(configPath()))))
		return nil
	}
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
