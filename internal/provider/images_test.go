package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// testPNG is a 1x1 PNG.
var testPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

// useImageStore makes a temporary directory the image store for one test.
func useImageStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	previous := ImageStore
	ImageStore = func() (string, error) { return dir, nil }
	t.Cleanup(func() { ImageStore = previous })
	return dir
}

// storedTestImage is a test PNG in the image store, which it sets up if needed.
func storedTestImage(t *testing.T) Image {
	t.Helper()
	dir := ""
	if ImageStore != nil {
		dir, _ = ImageStore()
	}
	if dir == "" {
		dir = useImageStore(t)
	}
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, testPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	return Image{Path: path, MediaType: "image/png"}
}

var testPNGBase64 = base64.StdEncoding.EncodeToString(testPNG)

func TestImageRefsRoundTrip(t *testing.T) {
	img := Image{Path: `C:\Users\me\shot.png`, MediaType: "image/png"}
	if got, ok := parseImageRef(img.ref()); !ok || got != img {
		t.Fatalf("got %+v %v", got, ok)
	}
	for _, text := range []string{"hello", imageRefPrefix + "image/png", imageRefPrefix + "image/png;"} {
		if _, ok := parseImageRef(text); ok {
			t.Errorf("%q read as a ref", text)
		}
	}
}

func TestExpandImageRefsFillsInTheData(t *testing.T) {
	store := useImageStore(t)
	img := storedTestImage(t)
	gone := Image{Path: filepath.Join(store, "gone.png"), MediaType: "image/png"}
	plain := json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"same bytes"}]}`)
	anthropicMessage, _ := newAnthropicUserMessage(userMessageContent{text: "look", userImages: []Image{img, gone}}, nil)
	chat, _ := chatUserMessage("look", []Image{img}, nil)

	out := expandImageRefs([]json.RawMessage{plain, anthropicMessage, chat}, false, anthropicImageBudget)
	if string(out[0]) != string(plain) {
		t.Errorf("a message without images changed: %s", out[0])
	}
	if strings.Contains(string(out[1]), imageRefPrefix) || !strings.Contains(string(out[1]), `"data":"`+testPNGBase64+`"`) {
		t.Errorf("Anthropic image not filled in: %s", out[1])
	}
	if !strings.Contains(string(out[1]), "gone.png is no longer available") {
		t.Errorf("a missing file should become a note: %s", out[1])
	}
	if !strings.Contains(string(out[2]), `"url":"data:image/png;base64,`+testPNGBase64+`"`) {
		t.Errorf("Chat image not filled in: %s", out[2])
	}
	// The stored messages keep only the reference.
	if !strings.Contains(string(anthropicMessage), imageRefPrefix) || strings.Contains(string(anthropicMessage), testPNGBase64) {
		t.Errorf("the stored message should hold the reference only: %s", anthropicMessage)
	}
	if CountImageRefs(Conversation{Messages: []json.RawMessage{anthropicMessage, chat}}) != 3 {
		t.Error("CountImageRefs")
	}
}

func TestAnthropicUserMessagePlacesToolImages(t *testing.T) {
	img := storedTestImage(t)
	results := []ToolResult{{CallID: "toolu_1", Output: "Image shot.png is attached.", Images: []Image{img}}}

	types := func(raw json.RawMessage) []string {
		var message struct {
			Content []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"content"`
		}
		json.Unmarshal(raw, &message)
		var out []string
		for _, block := range message.Content {
			kind := block.Type
			for _, inner := range block.Content {
				kind += "/" + inner.Type
			}
			out = append(out, kind)
		}
		return out
	}

	native, _ := newAnthropicUserMessage(userMessageContent{results: results, text: "next", toolImagesInResults: true}, nil)
	if got := strings.Join(types(native), " "); got != "tool_result/text/image text" {
		t.Errorf("Anthropic: %s", got)
	}
	loose, _ := newAnthropicUserMessage(userMessageContent{results: results, text: "next", userImages: []Image{img}}, nil)
	if got := strings.Join(types(loose), " "); got != "tool_result/text text image text image" {
		t.Errorf("other servers: %s", got)
	}
	if !strings.Contains(string(loose), toolImagesNote) {
		t.Errorf("the tool's images need an introduction: %s", loose)
	}
}

func TestChatProviderSendsToolImagesInAUserMessage(t *testing.T) {
	img := storedTestImage(t)
	server := newChatServer(t,
		[]string{chunk(`{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"view_image","arguments":"{\"path\":\"shot.png\"}"}}]}`, "tool_calls")},
		[]string{chunk(`{"role":"assistant","content":"A red dot."}`, "stop")},
	)
	p := newChatProvider("local", server.URL+"/v1", "")
	first, _, err := p.Send(context.Background(), Request{Model: "m", UserText: "what is in shot.png?"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := p.Send(context.Background(), Request{Model: "m", Conversation: first.Conversation, ToolResults: []ToolResult{{CallID: "call_a", Output: "Image shot.png is attached.", Images: []Image{img}}}}, nil)
	if err != nil || second.Text != "A red dot." {
		t.Fatalf("got %+v %v", second, err)
	}

	messages := server.bodies[1]["messages"].([]any)
	tool := messages[len(messages)-2].(map[string]any)
	user := messages[len(messages)-1].(map[string]any)
	if tool["role"] != "tool" || tool["content"] != "Image shot.png is attached." {
		t.Errorf("tool message: %v", tool)
	}
	parts := user["content"].([]any)
	if user["role"] != "user" || len(parts) != 2 || parts[0].(map[string]any)["text"] != toolImagesNote {
		t.Fatalf("user message: %v", user)
	}
	url := parts[1].(map[string]any)["image_url"].(map[string]any)["url"]
	if url != "data:image/png;base64,"+testPNGBase64 {
		t.Errorf("image url: %v", url)
	}
	// The kept transcript refers to the file rather than holding the data.
	stored, _ := json.Marshal(second.Conversation)
	if strings.Contains(string(stored), testPNGBase64) || !strings.Contains(string(stored), imageRefPrefix) {
		t.Errorf("transcript: %s", stored)
	}

	// Without images the user message stays plain text.
	if message, _ := chatUserMessage("hi", nil, nil); string(message) != `{"role":"user","content":"hi"}` {
		t.Errorf("got %s", message)
	}
	if message, _ := chatUserMessage("", nil, nil); message != nil {
		t.Errorf("got %s", message)
	}
}

func TestOpenAIInputCarriesImages(t *testing.T) {
	img := storedTestImage(t)
	gone := Image{Path: filepath.Join(t.TempDir(), "gone.png"), MediaType: "image/png"}
	input := openAIInput(Request{
		ToolResults: []ToolResult{{CallID: "call_a", Output: "attached", Images: []Image{img, gone}}, {CallID: "call_b", Output: "plain"}},
		UserText:    "and this one",
		UserImages:  []Image{img},
	})
	encoded, _ := json.Marshal(input)
	var items []map[string]any
	if err := json.Unmarshal(encoded, &items); err != nil || len(items) != 3 {
		t.Fatalf("items: %s", encoded)
	}
	output := items[0]["output"].([]any)
	if len(output) != 3 || output[0].(map[string]any)["type"] != "input_text" || output[1].(map[string]any)["image_url"] != "data:image/png;base64,"+testPNGBase64 || !strings.Contains(output[2].(map[string]any)["text"].(string), "no longer available") {
		t.Errorf("function_call_output: %v", output)
	}
	if items[1]["output"] != "plain" {
		t.Errorf("a result without images stays a string: %v", items[1])
	}
	content := items[2]["content"].([]any)
	if items[2]["role"] != "user" || len(content) != 2 || content[1].(map[string]any)["type"] != "input_image" {
		t.Errorf("user message: %v", items[2])
	}

	// A prompt with an image and no tool results is a list too.
	if list := openAIInput(Request{UserText: "look", UserImages: []Image{img}}); len(list.OfInputItemList) != 1 {
		t.Errorf("got %+v", list)
	}
	var _ responses.ResponseNewParamsInputUnion = input
}

func TestAcceptsImages(t *testing.T) {
	ResetEntries()
	t.Cleanup(ResetEntries)
	RegisterEntries(PresetEntries(func(string) bool { return false }))
	local, err := ParseEntries([]byte(`{"local": {"api": "openai", "base_url": "http://localhost:11434/v1", "images": ["llava:13b"]}, "proxy": {"api": "anthropic", "base_url": "http://localhost:4000", "images": true}, "plain": {"api": "openai", "base_url": "http://localhost:1234/v1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	RegisterEntries(local)

	for model, want := range map[Model]bool{
		{OpenAI, "gpt-6-luna"}:                        true,
		{OpenAI, "gpt-3.5-turbo"}:                     false,
		{Anthropic, "claude-opus-5-5"}:                true,
		{DeepSeek, "deepseek-v4-pro"}:                 false,
		{DeepSeek, "deepseek-v4-flash"}:               false,
		{DeepSeek, "deepseek-flash"}:                  true,
		{DeepSeek, "deepseek-v4.1-flash"}:             true,
		{DeepSeek, "deepseek-v4-flash-vision-exp"}:    true,
		{OpenCodeGo, "kimi-k3"}:                       true,
		{OpenCodeGo, "deepseek-v4-pro"}:               false,
		{"gemini", "gemini-3-pro"}:                    true,
		{"openrouter", "anthropic/claude-sonnet-5-5"}: true,
		{"groq", "llama-3.3-70b"}:                     false,
		{"local", "llava:13b"}:                        true,
		{"local", "qwen3-coder"}:                      false,
		{"proxy", "anything"}:                         true,
		{"plain", "llava:13b"}:                        false,
		{"nobody", "x"}:                               false,
	} {
		if got := AcceptsImages(model); got != want {
			t.Errorf("AcceptsImages(%s) = %v, want %v", model, got, want)
		}
	}

	for _, bad := range []string{`"yes"`, `[""]`, `{}`} {
		if _, err := ParseEntries([]byte(`{"x": {"api": "openai", "base_url": "http://localhost:1/v1", "images": ` + bad + `}}`)); err == nil || !strings.Contains(err.Error(), "x.images") {
			t.Errorf("images %s: got %v", bad, err)
		}
	}
}

// goat reads an image only from its own store, and only where it puts images itself.
func TestExpandImageRefsReadsOnlyGoatsImages(t *testing.T) {
	store := useImageStore(t)
	img := storedTestImage(t)
	secret := filepath.Join(t.TempDir(), "secret.png")
	os.WriteFile(secret, []byte("TOP-SECRET"), 0o600)
	link := filepath.Join(store, "link.png")
	if err := os.Symlink(secret, link); err != nil {
		t.Skip("cannot make symbolic links here:", err)
	}
	hidden := base64.StdEncoding.EncodeToString([]byte("TOP-SECRET"))
	block := func(path, mediaType string) map[string]any {
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": imageRefPrefix + mediaType + ";" + path}}
	}
	message := func(v any) json.RawMessage {
		data, _ := json.Marshal(v)
		return data
	}

	for name, test := range map[string]struct {
		message json.RawMessage
		sent    string
	}{
		"outside the store":       {message(map[string]any{"role": "user", "content": []any{block(secret, "image/png")}}), "secret.png is no longer available"},
		"a link out of the store": {message(map[string]any{"role": "user", "content": []any{block(link, "image/png")}}), "link.png is no longer available"},
		"not an image type":       {message(map[string]any{"role": "user", "content": []any{block(img.Path, "text/plain")}}), "shot.png is no longer available"},
		"a tool call's input": {message(map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t", "name": "read_dir", "input": map[string]any{"x": block(img.Path, "image/png")}},
		}}), imageRefPrefix},
		"deeper than a tool_result's content": {message(map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t", "content": []any{map[string]any{"type": "text", "text": "x", "extra": block(img.Path, "image/png")}}},
		}}), imageRefPrefix},
		"a Chat Completions assistant": {message(map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageRefPrefix + "image/png;" + img.Path}},
		}}), imageRefPrefix},
	} {
		out := string(expandImageRefs([]json.RawMessage{test.message}, false, anthropicImageBudget)[0])
		if strings.Contains(out, hidden) || strings.Contains(out, testPNGBase64) || !strings.Contains(out, test.sent) {
			t.Errorf("%s: sent %s", name, out)
		}
	}

	// goat's own places still get the data: a user message and a tool_result in one.
	own := message(map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "t", "content": []any{block(img.Path, "image/png")}},
		block(img.Path, "image/png"),
	}})
	if out := string(expandImageRefs([]json.RawMessage{own}, false, anthropicImageBudget)[0]); strings.Count(out, testPNGBase64) != 2 {
		t.Errorf("goat's own images were not filled in: %s", out)
	}

	// Without a store nothing is read.
	ImageStore = nil
	if out := string(expandImageRefs([]json.RawMessage{own}, false, anthropicImageBudget)[0]); strings.Contains(out, testPNGBase64) {
		t.Errorf("an image was read with no store set: %s", out)
	}
}

func TestExpandImageRefsSendsNotesToAModelThatCannotSee(t *testing.T) {
	useImageStore(t)
	img := storedTestImage(t)
	content := userMessageContent{
		results:    []ToolResult{{CallID: "toolu_1", Output: "Image shot.png", Images: []Image{img}}},
		text:       "look",
		userImages: []Image{img},
	}
	inResults, _ := newAnthropicUserMessage(content, nil)
	content.toolImagesInResults = true
	anthropicMessage, _ := newAnthropicUserMessage(content, nil)
	chat, _ := chatUserMessage("look", []Image{img}, nil)

	out := expandImageRefs([]json.RawMessage{anthropicMessage, chat, inResults}, true, anthropicImageBudget)
	for i, message := range out {
		text := string(message)
		if strings.Contains(text, testPNGBase64) || strings.Contains(text, `"type":"image"`) || strings.Contains(text, `"type":"image_url"`) {
			t.Errorf("message %d still holds an image: %s", i, text)
		}
		if !strings.Contains(text, textOnlyImageText) {
			t.Errorf("message %d should say an image was there: %s", i, text)
		}
	}
	// In Anthropic's own shape the note takes the image's place inside the tool_result.
	var parsed struct {
		Content []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out[0], &parsed); err != nil || parsed.Content[0].Type != "tool_result" {
		t.Fatalf("got %s (%v)", out[0], err)
	}
	if inner := parsed.Content[0].Content; len(inner) != 2 || inner[1].Type != "text" || inner[1].Text != textOnlyImageText {
		t.Errorf("tool_result content: %+v", inner)
	}
	for _, i := range []int{0, 2} {
		if strings.Count(string(out[i]), textOnlyImageText) != 2 {
			t.Errorf("both the tool's and the prompt's image need a note: %s", out[i])
		}
	}
}

// storedImages writes n distinct images to the image store, oldest first, and returns
// them with their base64 data.
func storedImages(t *testing.T, n int) ([]Image, []string) {
	t.Helper()
	dir := useImageStore(t)
	var images []Image
	var encoded []string
	for i := range n {
		data := append(append([]byte(nil), testPNG...), fmt.Sprintf("image %d", i)...)
		path := filepath.Join(dir, fmt.Sprintf("shot-%d.png", i))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		images = append(images, Image{Path: path, MediaType: "image/png"})
		encoded = append(encoded, base64.StdEncoding.EncodeToString(data))
	}
	return images, encoded
}

func TestExpandImageRefsKeepsTheNewestImagesWithinTheBudget(t *testing.T) {
	images, encoded := storedImages(t, 5)
	// One image per message, as a long session of screenshots would leave them: three
	// in Anthropic's shape (one inside a tool_result), then two in Chat Completions'.
	first, _ := newAnthropicUserMessage(userMessageContent{text: "a", userImages: images[:1]}, nil)
	second, _ := newAnthropicUserMessage(userMessageContent{results: []ToolResult{{CallID: "toolu_1", Output: "b", Images: images[1:2]}}, toolImagesInResults: true}, nil)
	third, _ := newAnthropicUserMessage(userMessageContent{text: "c", userImages: images[2:3]}, nil)
	fourth, _ := chatUserMessage("d", nil, images[3:4])
	fifth, _ := chatUserMessage("e", nil, images[4:5])
	messages := []json.RawMessage{first, second, third, fourth, fifth}

	size := base64.StdEncoding.EncodedLen(len(testPNG) + len("image 0"))
	for _, test := range []struct {
		name   string
		budget imageBudget
	}{
		{"by size", imageBudget{bytes: 3*size + size/2, count: 20}},
		{"by count", imageBudget{bytes: 1 << 30, count: 3}},
	} {
		out := expandImageRefs(messages, false, test.budget)
		all := ""
		for _, message := range out {
			all += string(message)
		}
		for i, data := range encoded {
			kept := strings.Contains(all, data)
			if want := i >= 2; kept != want {
				t.Errorf("%s: image %d sent = %v, want %v", test.name, i, kept, want)
			}
		}
		if strings.Count(all, leftOutImageText) != 2 {
			t.Errorf("%s: the two oldest images need a note:\n%s", test.name, all)
		}
		if strings.Contains(all, imageRefPrefix) {
			t.Errorf("%s: a reference was sent: %s", test.name, all)
		}
	}
	// The stored messages keep every reference.
	for i, message := range messages {
		if !strings.Contains(string(message), imageRefPrefix) {
			t.Errorf("stored message %d lost its image: %s", i, message)
		}
	}
}

func TestOpenAIInputKeepsTheNewestImagesWithinTheBudget(t *testing.T) {
	images, encoded := storedImages(t, 3)
	previous := responsesImageBudget
	responsesImageBudget = imageBudget{bytes: 1 << 30, count: 2}
	t.Cleanup(func() { responsesImageBudget = previous })

	req := Request{
		ToolResults: []ToolResult{{CallID: "call_a", Output: "shots", Images: images[:2]}},
		UserText:    "and this one",
		UserImages:  images[2:],
	}
	sent, _ := json.Marshal(openAIInput(req))
	for i, data := range encoded {
		if kept := strings.Contains(string(sent), data); kept != (i >= 1) {
			t.Errorf("image %d sent = %v", i, kept)
		}
	}
	if strings.Count(string(sent), leftOutImageText) != 1 {
		t.Errorf("the oldest image needs a note: %s", sent)
	}

	// A model that cannot see gets notes and no images.
	req.TextOnly = true
	sent, _ = json.Marshal(openAIInput(req))
	if strings.Contains(string(sent), "input_image") || strings.Count(string(sent), textOnlyImageText) != 3 {
		t.Errorf("text-only input: %s", sent)
	}
}

func TestIsRequestTooLarge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		io.WriteString(w, `{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum allowed number of bytes."}}`)
	}))
	t.Cleanup(server.Close)
	p := NewAnthropicWith(true, option.WithBaseURL(server.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0))
	_, _, err := p.Send(context.Background(), Request{Model: "claude-opus-5-5", UserText: "hi"}, nil)
	if err == nil || !IsRequestTooLarge(err) {
		t.Fatalf("a 413 should be recognised: %v", err)
	}
	if !IsRequestTooLarge(fmt.Errorf("send: %w", &openai.Error{StatusCode: http.StatusRequestEntityTooLarge})) {
		t.Error("an OpenAI 413 should be recognised")
	}
	if IsRequestTooLarge(&openai.Error{StatusCode: http.StatusBadRequest}) || IsRequestTooLarge(errors.New("413 in the text alone")) {
		t.Error("only a 413 status counts")
	}
}
