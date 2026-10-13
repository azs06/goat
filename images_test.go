package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/azs06/goat/internal/provider"
)

func writeTestPNG(t *testing.T, path string, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if path != "" {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// lastUserBlocks returns the content blocks of a request's last message.
func lastUserBlocks(t *testing.T, req recordedRequest) []map[string]any {
	t.Helper()
	messages := messagesOf(t, req)
	var blocks []map[string]any
	for _, block := range messages[len(messages)-1].(map[string]any)["content"].([]any) {
		blocks = append(blocks, block.(map[string]any))
	}
	return blocks
}

func TestViewImageSendsTheImageInsideTheToolResult(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	original := writeTestPNG(t, filepath.Join(workDir, "shot.png"), 40, 30)

	fake := newFakeAnthropic(t,
		newSSE("msg_1").toolUse("toolu_1", "view_image", `{"path":"shot.png"}`).stop("tool_use", ""),
		newSSE("msg_2").text("A red corner.").stop("end_turn", ""),
	)
	conv, err := sendPromptStream(context.Background(), fake.provider(true), "what is in shot.png?", conversationState{}, turnOptions{model: "claude-opus-5-5", vision: true})
	if err != nil {
		t.Fatal(err)
	}

	blocks := lastUserBlocks(t, fake.request(1))
	if blocks[0]["type"] != "tool_result" {
		t.Fatalf("blocks: %v", blocks)
	}
	content := blocks[0]["content"].([]any)
	if len(content) != 2 || !strings.Contains(content[0].(map[string]any)["text"].(string), "Image shot.png (40x30 PNG") {
		t.Fatalf("tool_result content: %v", content)
	}
	source := content[1].(map[string]any)["source"].(map[string]any)
	data, _ := base64.StdEncoding.DecodeString(source["data"].(string))
	if source["type"] != "base64" || source["media_type"] != "image/png" || !bytes.Equal(data, original) {
		t.Errorf("image source: %v", source)
	}

	// The saved conversation names the stored copy instead of holding the data.
	saved, _ := json.Marshal(conv.conversation)
	if strings.Contains(string(saved), source["data"].(string)) || !strings.Contains(string(saved), "goat-image:image/png;") {
		t.Errorf("the conversation holds the image data: %s", saved)
	}
	if provider.CountImageRefs(conv.conversation) != 1 {
		t.Error("expected one image in the conversation")
	}
	stored := regexp.MustCompile(`goat-image:image/png;([^"]+)`).FindStringSubmatch(string(saved))[1]
	if strings.HasPrefix(filepath.Clean(stored), workDir) {
		t.Errorf("the copy belongs in goat's state, not the workspace: %s", stored)
	}

	// The history counts the image, so the size estimate does.
	entries, _ := loadConversationHistoryEntries()
	found := false
	for _, entry := range entries {
		if entry.Type == "tool_output" && entry.ToolName == "view_image" && entry.Images == 1 {
			found = true
		}
	}
	if !found {
		t.Error("the tool output entry should count the image")
	}
}

func TestDeepSeekGetsToolImagesAfterTheResults(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	writeTestPNG(t, filepath.Join(workDir, "shot.png"), 8, 8)

	fake := newFakeAnthropic(t,
		deepSeekThinking(newSSE("ds_1"), "Look at it.").toolUse("call_1", "view_image", `{"path":"shot.png"}`).stop("tool_use", ""),
		newSSE("ds_2").text("Noise.").stop("end_turn", ""),
	)
	fake.deepseekModels = deepSeekModelList
	if _, err := sendPromptStream(context.Background(), newDeepSeekTestProvider(fake), "look", conversationState{}, turnOptions{model: "deepseek-flash", vision: true}); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, block := range lastUserBlocks(t, fake.request(fake.requestCount()-1)) {
		types = append(types, block["type"].(string))
		if block["type"] == "tool_result" {
			if content, _ := block["content"].([]any); len(content) != 1 {
				t.Errorf("DeepSeek's tool_result should hold text only: %v", content)
			}
		}
	}
	if strings.Join(types, " ") != "tool_result text image" {
		t.Errorf("got %v", types)
	}
}

func TestViewImageWithoutVisionTellsTheModel(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	writeTestPNG(t, filepath.Join(workDir, "shot.png"), 8, 8)

	fake := newFakeAnthropic(t,
		newSSE("msg_1").toolUse("toolu_1", "view_image", `{"path":"shot.png"}`).stop("tool_use", ""),
		newSSE("msg_2").text("I cannot see it.").stop("end_turn", ""),
	)
	if _, err := sendPromptStream(context.Background(), fake.provider(true), "look", conversationState{}, turnOptions{model: "claude-opus-5-5"}); err != nil {
		t.Fatal(err)
	}
	result := lastUserBlocks(t, fake.request(1))[0]
	if result["is_error"] != true || !strings.Contains(fake.request(1).raw, "cannot see images") || strings.Contains(fake.request(1).raw, `"type":"image"`) {
		t.Errorf("got %s", fake.request(1).raw)
	}
}

func TestPromptImagesGoWithTheMessage(t *testing.T) {
	workDir := t.TempDir()
	chdirForTest(t, workDir)
	fake := newFakeAnthropic(t, newSSE("msg_1").text("A gradient.").stop("end_turn", ""))
	cfg := seamConfig(t, fake.provider(true))
	sessionSaver = func(string, savedSession) error { return nil }
	t.Cleanup(func() { sessionSaver = writeSessionFile })

	label, err := attachImage(cfg, writeTestPNG(t, "", 10, 10), false)
	if err != nil || label != "[image 1]" {
		t.Fatalf("got %q %v", label, err)
	}
	unused, _ := attachImage(cfg, writeTestPNG(t, "", 12, 12), false)
	captureStdout(t, func() {
		if err := handleInput(cfg, "what is "+label+"?"); err != nil {
			t.Fatal(err)
		}
	})
	var types []string
	for _, block := range lastUserBlocks(t, fake.request(0)) {
		types = append(types, block["type"].(string))
	}
	if strings.Join(types, " ") != "text image" {
		t.Errorf("got %v", types)
	}
	if len(cfg.promptImages) != 0 || len(cfg.lastImages) != 1 || unused != "[image 2]" {
		t.Errorf("the image the message dropped should be let go: %+v %+v", cfg.promptImages, cfg.lastImages)
	}
}

func TestAttachImageNeedsAModelThatSees(t *testing.T) {
	cfg := seamConfig(t, nil)
	cfg.model = provider.Model{Provider: provider.DeepSeek, ID: "deepseek-v4-pro"}
	if _, err := attachImage(cfg, writeTestPNG(t, "", 4, 4), false); err == nil || !strings.Contains(err.Error(), "cannot see images") {
		t.Errorf("got %v", err)
	}
	cfg.model = provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"}
	if _, err := attachImage(cfg, []byte("not an image"), false); err == nil {
		t.Error("expected text to be refused")
	}
	if !strings.Contains(imagesStatusText(cfg.model), "read by this model") || !strings.Contains(imagesStatusText(provider.Model{Provider: provider.DeepSeek, ID: "deepseek-v4-pro"}), "not read") {
		t.Error("imagesStatusText")
	}
}

func TestTakePromptImagesKeepsOnlyNamedOrPasted(t *testing.T) {
	cfg := &config{promptImages: []promptImage{
		{label: "[image 1]", image: provider.Image{Path: "a"}},
		{label: "[image 2]", image: provider.Image{Path: "b"}},
		{label: "[image 3]", image: provider.Image{Path: "c"}, always: true},
	}}
	got := takePromptImages(cfg, "compare [image 2] with the other")
	if len(got) != 2 || got[0].Path != "b" || got[1].Path != "c" || cfg.promptImages != nil {
		t.Errorf("got %+v", got)
	}
}

func TestPastedImagePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My Shots")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "shot (1).png")
	writeTestPNG(t, path, 4, 4)
	os.WriteFile(filepath.Join(dir, "fake.png"), []byte("text"), 0o644)

	accepted := []string{path, "'" + path + "'", `"` + path + `"`, "file://" + path, "  " + path + "\t"}
	if filepath.Separator == '/' {
		accepted = append(accepted, strings.NewReplacer(" ", `\ `, "(", `\(`, ")", `\)`).Replace(path))
	}
	for _, text := range accepted {
		if _, ok := pastedImagePath(text); !ok {
			t.Errorf("%q should attach", text)
		}
	}
	for _, text := range []string{"shot.png", path + "\n" + path, filepath.Join(dir, "fake.png"), filepath.Join(dir, "missing.png"), dir, "look at " + path} {
		if _, ok := pastedImagePath(text); ok {
			t.Errorf("%q should stay text", text)
		}
	}
}

type fakeAttacher struct {
	pasteErr error
	pasted   []string
}

func (f *fakeAttacher) pasteClipboard() (string, error) {
	if f.pasteErr != nil {
		return "", f.pasteErr
	}
	return "[image 1]", nil
}

func (f *fakeAttacher) attachPasted(text string) (string, bool) {
	f.pasted = append(f.pasted, text)
	if strings.HasSuffix(text, ".png") {
		return "[image 2]", true
	}
	return "", false
}

func TestEditorAttachesImages(t *testing.T) {
	attacher := &fakeAttacher{}
	e := &promptEditor{images: attacher}
	for _, key := range []string{"l", "o", "o", "k", "\x16"} {
		e.handleKey(key)
	}
	if string(e.buf) != "look [image 1] " {
		t.Fatalf("Ctrl-V: %q", string(e.buf))
	}
	for _, key := range append([]string{pasteStart}, append(strings.Split("/tmp/a.png", ""), pasteEnd)...) {
		e.handleKey(key)
	}
	if string(e.buf) != "look [image 1] [image 2] " {
		t.Fatalf("pasted path: %q", string(e.buf))
	}
	for _, key := range append([]string{pasteStart}, append(strings.Split("plain", ""), pasteEnd)...) {
		e.handleKey(key)
	}
	if !strings.HasSuffix(string(e.buf), "plain") || attacher.pasted[1] != "plain" {
		t.Fatalf("plain paste: %q", string(e.buf))
	}

	attacher.pasteErr = errors.New("the clipboard holds no image")
	e.handleKey("\x1bv")
	if !strings.Contains(e.render("footer", 80), "the clipboard holds no image") {
		t.Error("the reason should replace the footer")
	}
	e.handleKey("x")
	if strings.Contains(e.render("footer", 80), "clipboard") {
		t.Error("the notice should go with the next key")
	}

	// Without an attacher, as in the fuzz test, Ctrl-V does nothing.
	plain := &promptEditor{}
	plain.handleKey("\x16")
	if len(plain.buf) != 0 {
		t.Error("Ctrl-V inserted text")
	}
}

func TestCompactionCountsImages(t *testing.T) {
	plain := []historyEntry{{Type: "user", Content: "look"}}
	withImage := []historyEntry{{Type: "user", Content: "look", Images: 2}}
	if estimateConversationTokens(withImage)-estimateConversationTokens(plain) < 3000 {
		t.Error("two images should count for about 3,200 tokens")
	}
	if !strings.Contains(historyEntryCompactionText(withImage[0]), "[2 image(s) attached]") {
		t.Errorf("got %q", historyEntryCompactionText(withImage[0]))
	}
}

func TestWithoutImagesDropsThemForAModelThatCannotSee(t *testing.T) {
	results := []provider.ToolResult{{CallID: "a", Images: []provider.Image{{Path: "x"}}}, {CallID: "b"}}
	stripped := withoutImages(results)
	if len(resultImages(stripped)) != 0 || len(results[0].Images) != 1 {
		t.Error("withoutImages should copy, not change the results")
	}
	plain := []provider.ToolResult{{CallID: "b"}}
	if got := withoutImages(plain); &got[0] != &plain[0] {
		t.Error("results without images are passed through")
	}
}

func TestStoreImageKeepsItInState(t *testing.T) {
	chdirForTest(t, t.TempDir())
	cfg := &config{model: provider.Model{Provider: provider.Anthropic, ID: "claude-opus-5-5"}}
	if _, err := attachImage(cfg, writeTestPNG(t, "", 4, 4), true); err != nil {
		t.Fatal(err)
	}
	root := statePath(t, "images")
	if !strings.HasPrefix(cfg.promptImages[0].image.Path, root) {
		t.Errorf("stored at %s, want under %s", cfg.promptImages[0].image.Path, root)
	}
}
