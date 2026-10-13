package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Image is a picture for the model: a file in goat's state directory, already scaled to
// what the APIs accept (internal/images prepares it).
type Image struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
}

// imageRefPrefix marks where a stored transcript refers to an image file instead of
// holding its data. Transcripts are saved in session.json and resent with every
// request, so they keep only the path; the data goes in when a request is built. The
// request is the same every time, so prompt caching still works.
const imageRefPrefix = "goat-image:"

// ref is how a stored transcript names the image: the prefix, the media type, a
// semicolon, then the path.
func (img Image) ref() string {
	return imageRefPrefix + img.MediaType + ";" + img.Path
}

// parseImageRef reads a ref back; ok is false for any other text.
func parseImageRef(ref string) (Image, bool) {
	rest, ok := strings.CutPrefix(ref, imageRefPrefix)
	if !ok {
		return Image{}, false
	}
	mediaType, path, ok := strings.Cut(rest, ";")
	if !ok || path == "" {
		return Image{}, false
	}
	return Image{Path: path, MediaType: mediaType}, true
}

// toolImagesNote introduces images a tool returned when they have to travel in a user
// message, on APIs whose tool results hold text only.
const toolImagesNote = "Attached image(s) from the tool results above:"

// ImageStore names the directory goat keeps its images in: view_image's copies and
// pasted images. goat sends an image only from there, so a path that reached a
// transcript some other way, such as inside a model's tool call, is never read. Unset,
// no image is sent. The tools package sets it.
var ImageStore func() (string, error)

// imageMediaTypes are the formats goat stores and the APIs take.
var imageMediaTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// errUntrustedImage refuses an image that is not a file in goat's image store.
var errUntrustedImage = errors.New("not one of goat's stored images")

// storedPath is where the image's file really is, once it is known to be a regular file
// inside the image store, through any symbolic links.
func (img Image) storedPath() (string, error) {
	if ImageStore == nil || !imageMediaTypes[img.MediaType] {
		return "", errUntrustedImage
	}
	dir, err := ImageStore()
	if err != nil {
		return "", err
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(img.Path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errUntrustedImage
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return "", errUntrustedImage
	}
	return path, nil
}

// load reads the image's data as base64, only from goat's image store.
func (img Image) load() (string, error) {
	path, err := img.storedPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func (img Image) dataURL() (string, error) {
	encoded, err := img.load()
	if err != nil {
		return "", err
	}
	return "data:" + img.MediaType + ";base64," + encoded, nil
}

func missingImageText(path string) string {
	return fmt.Sprintf("[image %s is no longer available]", filepath.Base(path))
}

// textOnlyImageText stands in for an image when the model cannot see images.
const textOnlyImageText = "[an image was here; the current model cannot see images]"

// expandImageRefs puts the data of the images a stored transcript refers to into the
// messages sent, but only where goat itself puts images: the content of a user message
// and, in Anthropic's format, the content of a tool_result inside one. Anything else
// that looks like an image, such as an object inside a model's tool call, is sent as
// stored and never read. Anthropic image blocks take the base64 data; Chat Completions
// image_url parts take a data URL. An image goat cannot send, or any image when
// textOnly is set, becomes a note in its place. Messages without images are sent byte
// for byte as stored.
func expandImageRefs(messages []json.RawMessage, textOnly bool) []json.RawMessage {
	var out []json.RawMessage
	for i, message := range messages {
		if !bytes.Contains(message, []byte(imageRefPrefix)) {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(message))
		decoder.UseNumber()
		var value map[string]any
		if decoder.Decode(&value) != nil || value["role"] != "user" {
			continue
		}
		blocks, ok := value["content"].([]any)
		if !ok {
			continue
		}
		for j, block := range blocks {
			blocks[j] = expandImageBlock(block, textOnly)
			if result, ok := blocks[j].(map[string]any); ok && result["type"] == "tool_result" {
				if inner, ok := result["content"].([]any); ok {
					for k := range inner {
						inner[k] = expandImageBlock(inner[k], textOnly)
					}
				}
			}
		}
		expanded, err := json.Marshal(value)
		if err != nil {
			continue
		}
		if out == nil {
			out = append([]json.RawMessage(nil), messages...)
		}
		out[i] = expanded
	}
	if out == nil {
		return messages
	}
	return out
}

// expandImageBlock fills in one content block if it is an image that refers to a stored
// file, or, when textOnly is set, replaces it with a note; any other block comes back
// as it was.
func expandImageBlock(block any, textOnly bool) any {
	v, ok := block.(map[string]any)
	if !ok {
		return block
	}
	switch v["type"] {
	case "image":
		// {"type": "image", "source": {"type": "base64", "media_type": ..., "data": ref}}
		source, ok := v["source"].(map[string]any)
		if !ok {
			return block
		}
		ref, ok := source["data"].(string)
		img, isRef := parseImageRef(ref)
		if !ok || !isRef {
			return block
		}
		if textOnly {
			return map[string]any{"type": "text", "text": textOnlyImageText}
		}
		encoded, err := img.load()
		if err != nil {
			return map[string]any{"type": "text", "text": missingImageText(img.Path)}
		}
		source["data"] = encoded
	case "image_url":
		// {"type": "image_url", "image_url": {"url": ref}}
		part, ok := v["image_url"].(map[string]any)
		if !ok {
			return block
		}
		ref, ok := part["url"].(string)
		img, isRef := parseImageRef(ref)
		if !ok || !isRef {
			return block
		}
		if textOnly {
			return map[string]any{"type": "text", "text": textOnlyImageText}
		}
		url, err := img.dataURL()
		if err != nil {
			return map[string]any{"type": "text", "text": missingImageText(img.Path)}
		}
		part["url"] = url
	}
	return v
}

// CountImageRefs counts the images a stored conversation holds, for size estimates.
func CountImageRefs(conv Conversation) int {
	count := 0
	for _, message := range conv.Messages {
		count += bytes.Count(message, []byte(imageRefPrefix))
	}
	return count
}

// toolResultImages collects the images of results, in order.
func toolResultImages(results []ToolResult) []Image {
	var all []Image
	for _, result := range results {
		all = append(all, result.Images...)
	}
	return all
}
