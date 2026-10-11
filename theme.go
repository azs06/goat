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
	if name := strings.TrimSpace(os.Getenv("GOAT_THEME")); name != "" {
		if theme, ok := style.Builtin(name); ok {
			style.Use(theme)
			return "the built-in " + strings.ToLower(name) + " theme"
		}
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
		name := strings.ToLower(strings.TrimSpace(args[0]))
		theme, ok := style.Builtin(name)
		if !ok {
			return fmt.Errorf("there is no built-in theme %q; the built-in themes are %s", args[0], strings.Join(style.BuiltinNames(), ", "))
		}
		style.Use(theme)
		fmt.Println(formatSuccess("Switched to the "+name+" theme for this session.") + style.Muted.Render(" To keep it, set GOAT_THEME="+name+" in "+displayPath(globalEnvPath())+"."))
		return nil
	}
	using := applyTheme()
	switch {
	case using == "":
		wanted, _ := themePath()
		fmt.Println("Using the default theme. " + style.Muted.Render(fmt.Sprintf("Put a theme in %s or set GOAT_THEME to use your own.", wanted)))
	case strings.HasPrefix(using, "the built-in "):
		fmt.Println("Using " + using + ".")
	default:
		fmt.Println(formatSuccess("Reloaded the theme from " + using))
	}
	fmt.Println(style.Muted.Render("Built-in themes: " + strings.Join(style.BuiltinNames(), ", ") + ". /theme <name> switches."))
	return nil
}
