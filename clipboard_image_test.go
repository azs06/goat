package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
)

// fakeClipboard replaces the clipboard tools: run answers each command, by name and
// arguments, and the commands are kept.
func fakeClipboard(t *testing.T, goos string, run func(name string, args []string) ([]byte, error)) *[]string {
	t.Helper()
	chdirForTest(t, t.TempDir())
	var calls []string
	previousRunner, previousGOOS := clipboardRunner, clipboardGOOS
	clipboardRunner = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return run(name, args)
	}
	clipboardGOOS = goos
	t.Cleanup(func() { clipboardRunner, clipboardGOOS = previousRunner, previousGOOS })
	return &calls
}

// scriptPath finds the file a clipboard script was told to write.
var scriptPath = regexp.MustCompile(`POSIX file "([^"]+)"|Save\('([^']+)'`)

func writeScriptTarget(t *testing.T, args []string, data []byte) {
	t.Helper()
	match := scriptPath.FindStringSubmatch(strings.Join(args, " "))
	if match == nil {
		t.Fatalf("no target file in %v", args)
	}
	os.WriteFile(match[1]+match[2], data, 0o600)
}

func TestMacClipboardImage(t *testing.T) {
	png := writeTestPNG(t, "", 6, 6)
	calls := fakeClipboard(t, "darwin", func(name string, args []string) ([]byte, error) {
		writeScriptTarget(t, args, png)
		return []byte("ok\n"), nil
	})
	data, err := readClipboardImage(context.Background())
	if err != nil || !bytes.Equal(data, png) || !strings.HasPrefix((*calls)[0], "osascript -e try") || !strings.Contains((*calls)[0], "«class PNGf»") {
		t.Fatalf("got %d bytes, %v, calls %v", len(data), err, *calls)
	}
	entries, _ := os.ReadDir(statePath(t, "images"))
	if len(entries) != 0 {
		t.Errorf("the temporary file should be gone: %v", entries)
	}

	fakeClipboard(t, "darwin", func(string, []string) ([]byte, error) { return []byte("none\n"), nil })
	if _, err := readClipboardImage(context.Background()); !errors.Is(err, errNoClipboardImage) {
		t.Errorf("got %v", err)
	}
}

func TestWindowsClipboardImage(t *testing.T) {
	png := writeTestPNG(t, "", 6, 6)
	calls := fakeClipboard(t, "windows", func(name string, args []string) ([]byte, error) {
		writeScriptTarget(t, args, png)
		return []byte("ok\r\n"), nil
	})
	if data, err := readClipboardImage(context.Background()); err != nil || !bytes.Equal(data, png) || !strings.HasPrefix((*calls)[0], "powershell -NoProfile -STA -Command") {
		t.Fatalf("got %v, calls %v", err, *calls)
	}
}

func TestLinuxClipboardImage(t *testing.T) {
	png := writeTestPNG(t, "", 6, 6)
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	calls := fakeClipboard(t, "linux", func(name string, args []string) ([]byte, error) {
		if args[0] == "--list-types" {
			return []byte("text/plain\nimage/jpeg\nimage/png\n"), nil
		}
		return png, nil
	})
	if data, err := readClipboardImage(context.Background()); err != nil || !bytes.Equal(data, png) {
		t.Fatalf("got %v", err)
	}
	if (*calls)[1] != "wl-paste --type image/png --no-newline" {
		t.Errorf("calls %v", *calls)
	}

	// Without Wayland, xclip; with no image type offered, no image.
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("XDG_SESSION_TYPE", "x11")
	calls = fakeClipboard(t, "linux", func(name string, args []string) ([]byte, error) {
		return []byte("TARGETS\nUTF8_STRING\n"), nil
	})
	if _, err := readClipboardImage(context.Background()); !errors.Is(err, errNoClipboardImage) || !strings.HasPrefix((*calls)[0], "xclip -selection clipboard -t TARGETS") {
		t.Errorf("got %v, calls %v", err, *calls)
	}

	fakeClipboard(t, "linux", func(string, []string) ([]byte, error) { return nil, exec.ErrNotFound })
	if _, err := readClipboardImage(context.Background()); err == nil || !strings.Contains(err.Error(), "xclip") {
		t.Errorf("got %v", err)
	}
}

func TestPreferredImageType(t *testing.T) {
	for types, want := range map[string]string{
		"text/plain image/png":   "image/png",
		"image/bmp image/jpeg":   "image/jpeg",
		"image/bmp":              "image/bmp",
		"text/plain UTF8_STRING": "",
	} {
		if got := preferredImageType(strings.Fields(types)); got != want {
			t.Errorf("%q: got %q, want %q", types, got, want)
		}
	}
}

func TestPasteCommandAttachesForTheNextMessage(t *testing.T) {
	png := writeTestPNG(t, "", 6, 6)
	fakeClipboard(t, "darwin", func(name string, args []string) ([]byte, error) {
		writeScriptTarget(t, args, png)
		return []byte("ok"), nil
	})
	cfg := &config{model: provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"}}
	out := captureStdout(t, func() {
		if err := commandPaste(context.Background(), cfg, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "[image 1]") || len(cfg.promptImages) != 1 || !cfg.promptImages[0].always {
		t.Errorf("got %q %+v", out, cfg.promptImages)
	}
	// /paste attaches to the next message even though it does not name the label.
	if images := takePromptImages(cfg, "what is this?"); len(images) != 1 {
		t.Errorf("got %v", images)
	}
	if label, err := (sessionImages{cfg}).pasteClipboard(); err != nil || label != "[image 2]" || cfg.promptImages[0].always {
		t.Errorf("Ctrl-V: %q %v", label, err)
	}
}

func TestQuoting(t *testing.T) {
	if appleScriptString(`a "b" \c`) != `"a \"b\" \\c"` || powerShellString("it's") != "'it''s'" {
		t.Error("quoting")
	}
}
