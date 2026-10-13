package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/azs06/goat/internal/images"
	"github.com/azs06/goat/internal/workspace"
)

// clipboardRunner runs a clipboard tool and returns its standard output. Tests replace
// it.
var clipboardRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && stderr.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, err
}

// clipboardGOOS is runtime.GOOS; tests set it to try the other platforms.
var clipboardGOOS = runtime.GOOS

// readClipboardImage returns the image on the clipboard, without cgo: through
// osascript on macOS, wl-paste or xclip on Linux, and PowerShell on Windows. It returns
// errNoClipboardImage when the clipboard holds something else.
func readClipboardImage(ctx context.Context) ([]byte, error) {
	switch clipboardGOOS {
	case "darwin":
		return readMacClipboardImage(ctx)
	case "windows":
		return readWindowsClipboardImage(ctx)
	default:
		return readUnixClipboardImage(ctx)
	}
}

// clipboardTempFile is a private file in goat's state directory for a clipboard tool
// that can only write an image to a file. The caller removes it.
func clipboardTempFile() (string, error) {
	root, err := workspace.EnsureStateRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, ".clipboard-*.png")
	if err != nil {
		return "", err
	}
	file.Close()
	return file.Name(), nil
}

// readMacClipboardImage asks AppleScript for the clipboard as PNG, which macOS converts
// any image to, and has it write the data to a file.
func readMacClipboardImage(ctx context.Context) ([]byte, error) {
	path, err := clipboardTempFile()
	if err != nil {
		return nil, err
	}
	defer os.Remove(path)
	script := []string{
		"try",
		"set png to (the clipboard as «class PNGf»)",
		"on error",
		`return "none"`,
		"end try",
		"set f to open for access (POSIX file " + appleScriptString(path) + ") with write permission",
		"set eof f to 0",
		"write png to f",
		"close access f",
		`return "ok"`,
	}
	var args []string
	for _, line := range script {
		args = append(args, "-e", line)
	}
	out, err := clipboardRunner(ctx, "osascript", args...)
	if err != nil {
		return nil, fmt.Errorf("read the clipboard: %w", err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		return nil, errNoClipboardImage
	}
	return readClipboardFile(path)
}

// readWindowsClipboardImage has PowerShell save the clipboard image as PNG.
func readWindowsClipboardImage(ctx context.Context) ([]byte, error) {
	path, err := clipboardTempFile()
	if err != nil {
		return nil, err
	}
	defer os.Remove(path)
	script := strings.Join([]string{
		"Add-Type -AssemblyName System.Windows.Forms",
		"Add-Type -AssemblyName System.Drawing",
		"$img = [System.Windows.Forms.Clipboard]::GetImage()",
		"if ($img) { $img.Save(" + powerShellString(path) + ", [System.Drawing.Imaging.ImageFormat]::Png); 'ok' } else { 'none' }",
	}, "; ")
	out, err := clipboardRunner(ctx, "powershell", "-NoProfile", "-STA", "-Command", script)
	if err != nil {
		return nil, fmt.Errorf("read the clipboard: %w", err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		return nil, errNoClipboardImage
	}
	return readClipboardFile(path)
}

// readUnixClipboardImage uses wl-paste under Wayland and xclip under X11, picking the
// first image type the clipboard offers.
func readUnixClipboardImage(ctx context.Context) ([]byte, error) {
	type tool struct {
		name      string
		listTypes []string
		read      func(mediaType string) []string
	}
	wayland := tool{"wl-paste", []string{"--list-types"}, func(t string) []string { return []string{"--type", t, "--no-newline"} }}
	x11 := tool{"xclip", []string{"-selection", "clipboard", "-t", "TARGETS", "-o"}, func(t string) []string { return []string{"-selection", "clipboard", "-t", t, "-o"} }}
	candidates := []tool{x11}
	if os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("XDG_SESSION_TYPE") == "wayland" {
		candidates = []tool{wayland, x11}
	}

	var firstErr error
	for _, candidate := range candidates {
		list, err := clipboardRunner(ctx, candidate.name, candidate.listTypes...)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		mediaType := preferredImageType(strings.Fields(string(list)))
		if mediaType == "" {
			return nil, errNoClipboardImage
		}
		data, err := clipboardRunner(ctx, candidate.name, candidate.read(mediaType)...)
		if err != nil {
			return nil, fmt.Errorf("read the clipboard: %w", err)
		}
		if len(data) == 0 {
			return nil, errNoClipboardImage
		}
		return data, nil
	}
	if errors.Is(firstErr, exec.ErrNotFound) {
		return nil, errors.New("reading an image from the clipboard needs wl-paste (wl-clipboard) or xclip")
	}
	return nil, fmt.Errorf("read the clipboard: %w", firstErr)
}

// preferredImageType picks the clipboard type to ask for: one of the formats the APIs
// take, PNG first, else any other image type, which Prepare then refuses or converts.
func preferredImageType(types []string) string {
	for _, preferred := range []string{"image/png", "image/jpeg", "image/webp", "image/gif"} {
		for _, t := range types {
			if strings.EqualFold(strings.TrimSpace(t), preferred) {
				return t
			}
		}
	}
	for _, t := range types {
		if strings.HasPrefix(strings.ToLower(t), "image/") {
			return t
		}
	}
	return ""
}

func readClipboardFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the clipboard image: %w", err)
	}
	if len(data) == 0 || images.MediaType(data) == "" {
		return nil, errNoClipboardImage
	}
	return data, nil
}

// appleScriptString quotes text for AppleScript.
func appleScriptString(text string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(text) + `"`
}

// powerShellString quotes text for PowerShell, where only a single quote needs
// doubling inside single quotes.
func powerShellString(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "''") + "'"
}
