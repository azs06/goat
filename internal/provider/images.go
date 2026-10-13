package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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

// load reads the image's data as base64.
func (img Image) load() (string, error) {
	data, err := os.ReadFile(img.Path)
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

// expandImageRefs puts the data of every image a stored transcript refers to into the
// messages sent. Anthropic image blocks take the base64 data; Chat Completions
// image_url parts take a data URL. A file that is gone becomes a note in its place.
// Messages without images are sent byte for byte as stored.
func expandImageRefs(messages []json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	for i, message := range messages {
		if !bytes.Contains(message, []byte(imageRefPrefix)) {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(message))
		decoder.UseNumber()
		var value any
		if decoder.Decode(&value) != nil {
			continue
		}
		expanded, err := json.Marshal(expandImageValue(value))
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

func expandImageValue(value any) any {
	switch v := value.(type) {
	case []any:
		for i := range v {
			v[i] = expandImageValue(v[i])
		}
		return v
	case map[string]any:
		switch v["type"] {
		case "image":
			// {"type": "image", "source": {"type": "base64", "media_type": ..., "data": ref}}
			if source, ok := v["source"].(map[string]any); ok {
				if ref, ok := source["data"].(string); ok {
					img, isRef := parseImageRef(ref)
					if !isRef {
						break
					}
					encoded, err := img.load()
					if err != nil {
						return map[string]any{"type": "text", "text": missingImageText(img.Path)}
					}
					source["data"] = encoded
					return v
				}
			}
		case "image_url":
			// {"type": "image_url", "image_url": {"url": ref}}
			if part, ok := v["image_url"].(map[string]any); ok {
				if ref, ok := part["url"].(string); ok {
					img, isRef := parseImageRef(ref)
					if !isRef {
						break
					}
					url, err := img.dataURL()
					if err != nil {
						return map[string]any{"type": "text", "text": missingImageText(img.Path)}
					}
					part["url"] = url
					return v
				}
			}
		}
		for key, inner := range v {
			v[key] = expandImageValue(inner)
		}
		return v
	}
	return value
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
