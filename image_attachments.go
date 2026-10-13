package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/azs06/goat/internal/images"
	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/style"
	"github.com/azs06/goat/internal/tools"
)

// promptImage is an image attached at the prompt, waiting for the message that names
// it. Its label, such as "[image 1]", stands for it in the text: deleting the label
// drops the image.
type promptImage struct {
	label string
	image provider.Image
	// always attaches the image to the next message whether or not it names the label,
	// as /paste does: with the plain line editor the label cannot be typed in for you.
	always bool
}

// errNoClipboardImage is reported when the clipboard holds no image.
var errNoClipboardImage = errors.New("the clipboard holds no image; copy one first, such as a screenshot")

// attachImage keeps data as an image for the next message and returns its label. A
// model that cannot see images gets none, so the user hears it now rather than after
// the message is sent.
func attachImage(cfg *config, data []byte, always bool) (string, error) {
	if !provider.AcceptsImages(cfg.model) {
		return "", fmt.Errorf("%s cannot see images; switch to a model that does with /model", cfg.model)
	}
	prepared, err := images.Prepare(data)
	if err != nil {
		return "", err
	}
	stored, err := tools.StoreImage(prepared)
	if err != nil {
		return "", fmt.Errorf("keep a copy of the image: %w", err)
	}
	cfg.imageCount++
	label := fmt.Sprintf("[image %d]", cfg.imageCount)
	cfg.promptImages = append(cfg.promptImages, promptImage{label: label, image: stored, always: always})
	return label, nil
}

// takePromptImages returns the images text names by label, and those /paste attached,
// in the order they were attached. Every waiting image is then let go: one whose label
// was deleted was not wanted.
func takePromptImages(cfg *config, text string) []provider.Image {
	var taken []provider.Image
	for _, attached := range cfg.promptImages {
		if attached.always || strings.Contains(text, attached.label) {
			taken = append(taken, attached.image)
		}
	}
	cfg.promptImages = nil
	return taken
}

// pasteClipboardImage attaches the image on the clipboard, for Ctrl-V in the prompt
// editor and /paste.
func pasteClipboardImage(cfg *config, always bool) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
	defer cancel()
	data, err := readClipboardImage(ctx)
	if err != nil {
		return "", err
	}
	return attachImage(cfg, data, always)
}

// pastedImagePath recognizes text pasted at the prompt that is one image file's path,
// as a terminal pastes a file dragged onto it: plain, quoted, with backslash-escaped
// spaces, or as a file:// URL. It returns the file's contents.
func pastedImagePath(text string) ([]byte, bool) {
	path := strings.TrimSpace(text)
	if path == "" || strings.ContainsAny(path, "\r\n") {
		return nil, false
	}
	if len(path) >= 2 && (path[0] == '\'' || path[0] == '"') && path[len(path)-1] == path[0] {
		path = path[1 : len(path)-1]
	} else if filepath.Separator == '/' {
		path = strings.NewReplacer(`\ `, " ", `\(`, "(", `\)`, ")", `\'`, "'", `\&`, "&").Replace(path)
	}
	path = strings.TrimPrefix(path, "file://")
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, "~/") {
		path = filepath.Join(home, path[2:])
	}
	if !filepath.IsAbs(path) {
		return nil, false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
	default:
		return nil, false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > images.MaxInputBytes {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil || images.MediaType(data) == "" {
		return nil, false
	}
	return data, true
}

// imagesStatusText says in /status whether the model reads images.
func imagesStatusText(model provider.Model) string {
	if provider.AcceptsImages(model) {
		return "read by this model (view_image, ctrl-v or /paste to attach)"
	}
	return style.Muted.Render("not read by this model")
}

// sessionImages attaches images at the prompt editor to the session's next message.
type sessionImages struct{ cfg *config }

func (s sessionImages) pasteClipboard() (string, error) {
	return pasteClipboardImage(s.cfg, false)
}

// attachPasted attaches a pasted image path. A path that cannot be attached, as when
// the model cannot see images, stays the text that was pasted.
func (s sessionImages) attachPasted(text string) (string, bool) {
	data, ok := pastedImagePath(text)
	if !ok {
		return "", false
	}
	label, err := attachImage(s.cfg, data, false)
	return label, err == nil
}

// clipboardTimeout bounds reading the clipboard; a clipboard tool waiting on a display
// that is not there must not hang the prompt.
const clipboardTimeout = 5 * time.Second

func commandPaste(ctx context.Context, c *config, args []string) error {
	label, err := pasteClipboardImage(c, true)
	if err != nil {
		return err
	}
	fmt.Println(formatSuccess(fmt.Sprintf("Attached the clipboard image as %s; it goes with your next message.", label)))
	return nil
}
