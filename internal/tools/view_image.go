package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/azs06/goat/internal/images"
	"github.com/azs06/goat/internal/provider"
	"github.com/azs06/goat/internal/workspace"
)

// ViewImage is the tool that shows the model an image file from the workspace.
const ViewImage = "view_image"

// ErrNoVision is the error view_image gives when the model in use cannot see images.
var ErrNoVision = errors.New("the model in use cannot see images; describe what you need from the image another way, or ask the user to switch to a model that reads images with /model")

func viewImageSpec() provider.ToolSpec {
	return functionTool(ViewImage, "Look at an image file in the workspace (PNG, JPEG, GIF or WebP), such as a screenshot a test or a browser tool saved, to check what it shows. Large images are scaled down. Works only when the model in use reads images.",
		map[string]any{"path": stringParam}, "path")
}

func runViewImage(_ context.Context, raw string, opts Options) (string, error) {
	args, err := decodeArgs[struct {
		Path string `json:"path"`
	}](raw)
	if err != nil {
		return "", err
	}
	if !opts.Vision {
		return "", ErrNoVision
	}
	if opts.AttachImage == nil {
		return "", errors.New("images cannot be shown here")
	}
	resolved, _, err := workspace.ResolvePath(args.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", filepath.Clean(args.Path))
	}
	if info.Size() > images.MaxInputBytes {
		return "", fmt.Errorf("%s is %d bytes, over the %d byte image limit", filepath.Clean(args.Path), info.Size(), images.MaxInputBytes)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", err
	}
	prepared, err := images.Prepare(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Clean(args.Path), err)
	}
	stored, err := StoreImage(prepared)
	if err != nil {
		return "", fmt.Errorf("keep a copy of the image: %w", err)
	}
	opts.AttachImage(stored)
	return fmt.Sprintf("Image %s (%s) is attached.", filepath.Clean(args.Path), prepared.Describe()), nil
}

// The provider package sends images only from ImageDir; it cannot import workspace
// itself.
func init() {
	provider.ImageStore = ImageDir
}

// ImageDir is the private directory in the workspace's state directory where goat keeps
// the images a conversation refers to.
func ImageDir() (string, error) {
	root, err := workspace.StateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "images"), nil
}

// StoreImage keeps a prepared image in ImageDir, where the conversation refers to it.
func StoreImage(prepared images.Prepared) (provider.Image, error) {
	if _, err := workspace.EnsureStateRoot(); err != nil {
		return provider.Image{}, err
	}
	dir, err := ImageDir()
	if err != nil {
		return provider.Image{}, err
	}
	path, err := images.Store(dir, prepared)
	if err != nil {
		return provider.Image{}, err
	}
	return provider.Image{Path: path, MediaType: prepared.MediaType}, nil
}
