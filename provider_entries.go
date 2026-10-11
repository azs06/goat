package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

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

// loadProviderEntries registers the providers config.json adds (fromConfig) and those
// in providers.json, the older file for them. A file goat cannot open is skipped with a
// warning; one it cannot parse stops goat, naming the file and line without quoting it.
// A provider both files define is a mistake too.
func loadProviderEntries(fromConfig []provider.Entry) (warnings []string, err error) {
	entries := fromConfig
	if path := providerEntriesPath(); path != "" {
		data, readErr := os.ReadFile(path)
		switch {
		case errors.Is(readErr, fs.ErrNotExist):
		case readErr != nil:
			warnings = append(warnings, fmt.Sprintf("skipping %s: %v", displayPath(path), readErr))
		default:
			more, err := provider.ParseEntries(data)
			if err != nil {
				return warnings, fmt.Errorf("%s: %w", displayPath(path), err)
			}
			for i := range more {
				more[i].Source = displayPath(path)
			}
			for _, entry := range more {
				if slices.ContainsFunc(entries, func(e provider.Entry) bool { return e.Name == entry.Name }) {
					return warnings, fmt.Errorf("%s defines %s, which config.json's providers define too; keep one", displayPath(path), entry.Name)
				}
			}
			entries = append(entries, more...)
		}
	}
	provider.RegisterEntries(entries)
	return warnings, nil
}
