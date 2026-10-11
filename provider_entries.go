package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/azs06/goat/internal/provider"
)

// providerEntriesPath is the providers.json in goat's config directory, or "" when there
// is no config directory. Like the global .env it is never read from the workspace: an
// entry decides where a key is sent.
func providerEntriesPath() string {
	if dir := goatConfigDir(); dir != "" {
		return filepath.Join(dir, "providers.json")
	}
	return ""
}

// loadProviderEntries registers the providers the user adds in providers.json. A file
// goat cannot open is skipped with a warning; one it cannot parse stops goat, naming the
// file and line without quoting it.
func loadProviderEntries() (warnings []string, err error) {
	path := providerEntriesPath()
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return []string{fmt.Sprintf("skipping %s: %v", displayPath(path), err)}, nil
	}
	entries, err := provider.ParseEntries(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", displayPath(path), err)
	}
	provider.RegisterEntries(entries)
	return nil, nil
}
