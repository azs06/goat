package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
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

func missingImageText(path string) string {
	return fmt.Sprintf("[image %s is no longer available]", filepath.Base(path))
}

// textOnlyImageText stands in for an image when the model cannot see images.
const textOnlyImageText = "[an image was here; the current model cannot see images]"

// leftOutImageText stands in for an older image that did not fit a request's budget.
const leftOutImageText = "[an earlier image was left out to keep the request small]"

// imageBudget bounds the images one request carries, by their base64 size and their
// number. Each image fits the APIs by itself, but enough of them can pass a request
// size limit, which the server answers with an error the conversation cannot get past:
// every later request would carry them again. The newest images go; older ones become
// a note. Twenty images also stay under Anthropic's lower size limit for requests with
// more than twenty.
type imageBudget struct {
	bytes int
	count int
}

var (
	// Anthropic takes requests up to 32 MB; DeepSeek's Messages endpoint 48 MiB.
	anthropicImageBudget = imageBudget{bytes: 20 << 20, count: 20}
	// The Responses API takes up to 50 MB of images in one request.
	responsesImageBudget = imageBudget{bytes: 20 << 20, count: 20}
	// Chat Completions servers vary, and most document no limit.
	chatImageBudget = imageBudget{bytes: 10 << 20, count: 20}
)

// plannedImage is what goes in one image's place in a request: its base64 data, or a
// note when it cannot or need not go.
type plannedImage struct {
	encoded string
	note    string
}

// planImages decides, newest first, which of a request's images go, reading them only
// from goat's image store. With textOnly set none go. An image goat cannot read gets a
// note, and once the budget is spent every older image gets one too.
func planImages(images []Image, textOnly bool, budget imageBudget) []plannedImage {
	plans := make([]plannedImage, len(images))
	count, used, full := 0, 0, false
	for i := len(images) - 1; i >= 0; i-- {
		img := images[i]
		if textOnly {
			plans[i].note = textOnlyImageText
			continue
		}
		path, err := img.storedPath()
		var info os.FileInfo
		if err == nil {
			info, err = os.Stat(path)
		}
		if err != nil {
			plans[i].note = missingImageText(img.Path)
			continue
		}
		size := base64.StdEncoding.EncodedLen(int(info.Size()))
		if full || count >= budget.count || used+size > budget.bytes {
			full = true
			plans[i].note = leftOutImageText
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			plans[i].note = missingImageText(img.Path)
			continue
		}
		count++
		used += size
		plans[i].encoded = base64.StdEncoding.EncodeToString(data)
	}
	return plans
}

// expandImageRefs puts the data of the images a stored transcript refers to into the
// messages sent, but only where goat itself puts images: the content of a user message
// and, in Anthropic's format, the content of a tool_result inside one. Anything else
// that looks like an image, such as an object inside a model's tool call, is sent as
// stored and never read. Anthropic image blocks take the base64 data; Chat Completions
// image_url parts take a data URL. The newest images go while they fit the budget; an
// image goat cannot send, an older one past the budget, or any image when textOnly is
// set becomes a note in its place. Messages without images are sent byte for byte as
// stored.
func expandImageRefs(messages []json.RawMessage, textOnly bool, budget imageBudget) []json.RawMessage {
	// slot is where one image block sits: blocks[index].
	type slot struct {
		blocks []any
		index  int
	}
	var slots []slot
	var images []Image
	collect := func(blocks []any, index int) {
		if img, ok := imageRefBlock(blocks[index]); ok {
			slots = append(slots, slot{blocks, index})
			images = append(images, img)
		}
	}
	decoded := map[int]map[string]any{}
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
		decoded[i] = value
		// In the order the model reads them, so the budget keeps the newest.
		for j, block := range blocks {
			if result, ok := block.(map[string]any); ok && result["type"] == "tool_result" {
				if inner, ok := result["content"].([]any); ok {
					for k := range inner {
						collect(inner, k)
					}
				}
				continue
			}
			collect(blocks, j)
		}
	}
	if len(decoded) == 0 {
		return messages
	}

	for k, plan := range planImages(images, textOnly, budget) {
		s := slots[k]
		if plan.note != "" {
			s.blocks[s.index] = map[string]any{"type": "text", "text": plan.note}
			continue
		}
		fillImageBlock(s.blocks[s.index].(map[string]any), images[k], plan.encoded)
	}

	out := append([]json.RawMessage(nil), messages...)
	for i, value := range decoded {
		if expanded, err := json.Marshal(value); err == nil {
			out[i] = expanded
		}
	}
	return out
}

// imageRefBlock reports a content block that is an image referring to a stored file:
// an Anthropic image block or a Chat Completions image_url part.
func imageRefBlock(block any) (Image, bool) {
	v, ok := block.(map[string]any)
	if !ok {
		return Image{}, false
	}
	var ref string
	switch v["type"] {
	case "image":
		// {"type": "image", "source": {"type": "base64", "media_type": ..., "data": ref}}
		source, _ := v["source"].(map[string]any)
		ref, _ = source["data"].(string)
	case "image_url":
		// {"type": "image_url", "image_url": {"url": ref}}
		part, _ := v["image_url"].(map[string]any)
		ref, _ = part["url"].(string)
	default:
		return Image{}, false
	}
	return parseImageRef(ref)
}

// fillImageBlock puts an image's base64 data into its block in place of the reference.
func fillImageBlock(block map[string]any, img Image, encoded string) {
	switch block["type"] {
	case "image":
		block["source"].(map[string]any)["data"] = encoded
	case "image_url":
		block["image_url"].(map[string]any)["url"] = dataURL(img, encoded)
	}
}

// dataURL is an image's base64 data as a data URL.
func dataURL(img Image, encoded string) string {
	return "data:" + img.MediaType + ";base64," + encoded
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

// IsRequestTooLarge reports a server refusing a request for its size (HTTP 413), as one
// carrying many images can be. A compacted conversation carries none of them.
func IsRequestTooLarge(err error) bool {
	var anthropicErr *anthropic.Error
	if errors.As(err, &anthropicErr) {
		return anthropicErr.StatusCode == http.StatusRequestEntityTooLarge
	}
	var openaiErr *openai.Error
	if errors.As(err, &openaiErr) {
		return openaiErr.StatusCode == http.StatusRequestEntityTooLarge
	}
	return false
}
