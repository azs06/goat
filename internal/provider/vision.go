package provider

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// AcceptsImages reports whether goat sends images to model. Built-in providers and
// presets go by what each model family is documented to take; an entry from the user's
// files takes images only when its "images" setting says so, since a local server's
// model of the same name may not. A model that does not get images is told so instead.
func AcceptsImages(model Model) bool {
	id := strings.ToLower(model.ID)
	switch model.Provider {
	case OpenAI:
		return openAIAcceptsImages(id)
	case Anthropic:
		return strings.HasPrefix(id, "claude-")
	case DeepSeek:
		return deepSeekAcceptsImages(id)
	}
	entry, ok := entries[model.Provider]
	if !ok {
		return false
	}
	if entry.Images != nil {
		return entry.Images.accepts(model.ID)
	}
	if isOpenCode(model.Provider) {
		return openCodeVisionModels[id]
	}
	if entry.Preset {
		return presetAcceptsImages(id)
	}
	return false
}

// openAIAcceptsImages is false for the few OpenAI chat models without vision.
func openAIAcceptsImages(id string) bool {
	for _, prefix := range []string{"gpt-3.5", "o1-mini", "o3-mini", "gpt-4-0", "gpt-4-32k"} {
		if strings.HasPrefix(id, prefix) {
			return false
		}
	}
	return id != "gpt-4"
}

// deepSeekAcceptsImages follows DeepSeek's docs (2026-10): deepseek-flash, V4.1 Flash
// and later read images, and so does the retired vision preview, which now routes to
// Flash; the Pro models and V4 Flash do not.
// https://api-docs.deepseek.com/guides/vision/
func deepSeekAcceptsImages(id string) bool {
	if id == "deepseek-flash" || strings.Contains(id, "vision") {
		return true
	}
	return strings.Contains(id, "-flash") && compareVersions(deepSeekVersion(id), []int{4, 1}) >= 0
}

// openCodeVisionModels are the OpenCode Go models that take images, from pi's catalog
// (2026-10).
var openCodeVisionModels = map[string]bool{
	"claude-haiku-5-5": true, "deepseek-v4.1-flash": true, "deepseek-v4-flash-vision-exp": true,
	"glm-5.3-flash": true, "kimi-k3": true, "kimi-k2.7-code": true, "minimax-m3": true,
	"mimo-v2.6-pro": true, "mimo-v2.6-flash": true, "mimo-v2.5": true,
	"qwen3.8-max": true, "qwen3.8-flash": true, "qwen3.7-plus": true,
	"longcat-2.5-preview-free": true, "space-bunny": true,
}

// presetAcceptsImages guesses for the presets' many models by family. Model names that
// say "vision" or "-vl" always count.
func presetAcceptsImages(id string) bool {
	if strings.Contains(id, "vision") || strings.Contains(id, "-vl") || strings.Contains(id, "pixtral") {
		return true
	}
	// OpenRouter names models provider/model.
	if _, rest, ok := strings.Cut(id, "/"); ok {
		id = rest
	}
	for _, prefix := range []string{"claude-", "gemini-", "gpt-4o", "gpt-4.1", "gpt-5", "gpt-6", "grok-4", "kimi-k3", "llama-4", "mistral-medium", "mistral-small-3", "qwen3.8", "qwen3.7", "glm-5.3-flash"} {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

// entryImages is an entry's "images" setting: true or false for all its models, or a
// list of the models that take images.
type entryImages struct {
	all    bool
	models []string
}

// parseEntryImages reads "images": true, false, or a list of model names.
func parseEntryImages(raw json.RawMessage) (*entryImages, error) {
	var all bool
	if json.Unmarshal(raw, &all) == nil {
		return &entryImages{all: all}, nil
	}
	var models []string
	if json.Unmarshal(raw, &models) != nil {
		return nil, errors.New("must be true, false, or a list of the models that take images")
	}
	for i, model := range models {
		models[i] = strings.TrimSpace(model)
		if models[i] == "" {
			return nil, errors.New("has an empty model name")
		}
	}
	return &entryImages{models: models}, nil
}

func (e *entryImages) accepts(model string) bool {
	return e.all || slices.ContainsFunc(e.models, func(m string) bool { return strings.EqualFold(m, model) })
}
