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

// applyTheme loads the theme file, if there is one, over the built-in theme and makes
// it current. Declarations that cannot be used are warned about and skipped; a file
// that cannot be read or is not valid JSON leaves the built-in theme in place. It
// returns the file in use, or "" for the built-in theme.
func applyTheme() string {
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

func commandTheme(ctx context.Context, c *config, args []string) error {
	path := applyTheme()
	if path == "" {
		wanted, _ := themePath()
		fmt.Println("Using the built-in theme. " + style.Muted.Render(fmt.Sprintf("Put a theme in %s or set GOAT_THEME to use your own.", wanted)))
		return nil
	}
	fmt.Println(formatSuccess("Reloaded the theme from " + path))
	return nil
}
