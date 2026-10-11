package provider

import "slices"

// preset is a provider goat knows without config.json beyond the built-in three: an
// OpenAI-compatible API that needs only a key. Presets are registered like entries,
// after the user's own, and an entry of the same name in config.json replaces one.
type preset struct {
	name, label, baseURL, keyEnv string
}

// presets are listed in the order /login offers them. Their URLs come from each
// provider's documentation of its OpenAI-compatible API.
var presets = []preset{
	{"openrouter", "OpenRouter", "https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"},
	{"gemini", "Google Gemini", "https://generativelanguage.googleapis.com/v1beta/openai", "GEMINI_API_KEY"},
	{"xai", "xAI (Grok)", "https://api.x.ai/v1", "XAI_API_KEY"},
	{"mistral", "Mistral", "https://api.mistral.ai/v1", "MISTRAL_API_KEY"},
	{"groq", "Groq", "https://api.groq.com/openai/v1", "GROQ_API_KEY"},
	{"together", "Together AI", "https://api.together.ai/v1", "TOGETHER_API_KEY"},
	{"fireworks", "Fireworks AI", "https://api.fireworks.ai/inference/v1", "FIREWORKS_API_KEY"},
	{"cerebras", "Cerebras", "https://api.cerebras.ai/v1", "CEREBRAS_API_KEY"},
	{"moonshot", "Moonshot (Kimi)", "https://api.moonshot.ai/v1", "MOONSHOT_API_KEY"},
	{"zai", "Z.ai (GLM)", "https://api.z.ai/api/paas/v4", "ZAI_API_KEY"},
	{"qwen", "Qwen (Alibaba Model Studio)", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", "DASHSCOPE_API_KEY"},
	{"minimax", "MiniMax", "https://api.minimax.io/v1", "MINIMAX_API_KEY"},
}

// builtInLabels are the built-in providers' names as people write them.
var builtInLabels = map[string]string{OpenAI: "OpenAI", Anthropic: "Anthropic", DeepSeek: "DeepSeek"}

// PresetEntries are the presets that taken does not claim, as entries ready to register.
// They list no models: the picker asks each server for its own.
func PresetEntries(taken func(name string) bool) []Entry {
	var list []Entry
	for _, p := range presets {
		if taken(p.name) {
			continue
		}
		endpoint, err := checkBaseURL(p.name+".base_url", p.baseURL, p.baseURL)
		if err != nil {
			panic(err)
		}
		list = append(list, Entry{Name: p.name, API: APIOpenAI, BaseURL: endpoint.URL, KeyEnv: p.keyEnv, Preset: true, endpoint: endpoint})
	}
	return list
}

// IsPreset reports a provider registered from goat's presets rather than config.json.
func IsPreset(name string) bool {
	entry, ok := entries[name]
	return ok && entry.Preset
}

// Label is the provider's name as people write it, such as "OpenAI" or "Google Gemini";
// an entry from config.json goes by its own name.
func Label(name string) string {
	if label, ok := builtInLabels[name]; ok {
		return label
	}
	if IsPreset(name) {
		if i := slices.IndexFunc(presets, func(p preset) bool { return p.name == name }); i >= 0 {
			return presets[i].label
		}
	}
	return name
}

// PresetKeyVars are the variables that hold the presets' keys.
func PresetKeyVars() []string {
	vars := make([]string, 0, len(presets))
	for _, p := range presets {
		vars = append(vars, p.keyEnv)
	}
	return vars
}
