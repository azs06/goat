package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// openCodeServer is a fake OpenCode Go: Chat Completions and the model list under /v1,
// and the Messages API beside them. It records each request's path and headers.
type openCodeServer struct {
	*httptest.Server
	mu      sync.Mutex
	paths   []string
	headers []http.Header
	bodies  []map[string]any
}

func newOpenCodeServer(t *testing.T) *openCodeServer {
	t.Helper()
	server := &openCodeServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		server.mu.Lock()
		server.paths = append(server.paths, r.URL.Path)
		server.headers = append(server.headers, r.Header.Clone())
		server.bodies = append(server.bodies, body)
		server.mu.Unlock()
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"object":"list","data":[{"id":"kimi-k3","object":"model"},{"id":"gpt-6-luna","object":"model"},{"id":"minimax-m3","object":"model"},{"id":"grok-4.7","object":"model"}]}`)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			for _, c := range []string{chunk(`{"role":"assistant","content":"from chat"}`, ""), chunk(`{}`, "stop"), usageChunk} {
				fmt.Fprintf(w, "data: %s\n\n", c)
			}
			io.WriteString(w, "data: [DONE]\n\n")
		case "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range []string{
				`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"minimax-m3","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"from messages"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
				`{"type":"message_stop"}`,
			} {
				var typed struct{ Type string }
				json.Unmarshal([]byte(event), &typed)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typed.Type, event)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// openCodeTest registers opencode-go as the preset does, but at baseURL.
func openCodeTest(t *testing.T, baseURL string) Provider {
	t.Helper()
	ResetEntries()
	t.Cleanup(ResetEntries)
	RegisterEntries([]Entry{{Name: OpenCodeGo, API: APIOpenCode, BaseURL: baseURL, KeyEnv: "OPENCODE_API_KEY", Preset: true}})
	t.Setenv("OPENCODE_API_KEY", "oc-key")
	p, err := NewEntry(OpenCodeGo)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenCodeGoPreset(t *testing.T) {
	ResetEntries()
	t.Cleanup(ResetEntries)
	RegisterEntries(PresetEntries(func(string) bool { return false }))
	entry, ok := LookupEntry(OpenCodeGo)
	if !ok || entry.API != APIOpenCode || entry.BaseURL != "https://opencode.ai/zen/go/v1" || entry.KeyEnv != "OPENCODE_API_KEY" || !entry.Preset {
		t.Fatalf("got %+v", entry)
	}
	if Label(OpenCodeGo) != "OpenCode Go" || SignIn(OpenCodeGo) != "" || !slices.Contains(PresetKeyVars(), "OPENCODE_API_KEY") {
		t.Fatal("label, sign-in or key variable")
	}
	if other, _ := LookupEntry("groq"); other.API != APIOpenAI {
		t.Fatalf("the other presets stay on Chat Completions, got %+v", other)
	}
}

func TestOpenCodeGoRoutesEachModelToItsAPI(t *testing.T) {
	server := newOpenCodeServer(t)
	p := openCodeTest(t, server.URL+"/v1")

	resp, _, err := p.Send(context.Background(), Request{Model: "kimi-k3", UserText: "hi"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "from chat" || resp.Conversation.Provider != OpenCodeGo || resp.Conversation.Format != FormatChat {
		t.Fatalf("got %+v", resp)
	}
	resp, _, err = p.Send(context.Background(), Request{Model: "minimax-m3", UserText: "hi", Effort: EffortHigh}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "from messages" || resp.Conversation.Provider != OpenCodeGo || resp.Conversation.Format != "" {
		t.Fatalf("got %+v", resp)
	}
	if _, err := p.Complete(context.Background(), "qwen3.8-max", "system", "summarize"); err != nil {
		t.Fatal(err)
	}

	if want := []string{"/v1/chat/completions", "/v1/messages", "/v1/messages"}; !slices.Equal(server.paths, want) {
		t.Fatalf("paths %v, want %v", server.paths, want)
	}
	session := server.headers[0].Get("x-opencode-session")
	if session == "" {
		t.Fatal("no x-opencode-session header")
	}
	for i, header := range server.headers {
		if header.Get("x-opencode-session") != session {
			t.Errorf("request %d: session %q, want %q", i, header.Get("x-opencode-session"), session)
		}
	}
	if server.headers[0].Get("Authorization") != "Bearer oc-key" || server.headers[1].Get("X-Api-Key") != "oc-key" {
		t.Fatal("the key must go to both APIs")
	}
	// Thinking settings go only to Claude models, as for any Anthropic-compatible entry.
	if _, ok := server.bodies[1]["thinking"]; ok {
		t.Fatalf("minimax-m3 got thinking settings: %v", server.bodies[1])
	}
}

func TestOpenCodeGoRefusesResponsesModels(t *testing.T) {
	server := newOpenCodeServer(t)
	p := openCodeTest(t, server.URL+"/v1")
	for _, model := range []string{"gpt-6-luna", "grok-4.7", "muse-spark-1.3-contributor"} {
		if _, _, err := p.Send(context.Background(), Request{Model: model, UserText: "hi"}, nil); err == nil || !strings.Contains(err.Error(), "Responses API") {
			t.Errorf("%s: got %v", model, err)
		}
		if err := CheckModel(context.Background(), p, model); err == nil {
			t.Errorf("%s: CheckModel let it through", model)
		}
	}
	if err := CheckModel(context.Background(), p, "kimi-k3"); err != nil {
		t.Fatal(err)
	}
	if len(server.paths) != 0 {
		t.Fatalf("no request should be made, got %v", server.paths)
	}

	ids, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"kimi-k3", "minimax-m3"}) {
		t.Fatalf("got %v", ids)
	}
}

func TestOpenCodeGoConversationFitsOnlyModelsOnTheSameAPI(t *testing.T) {
	openCodeTest(t, "https://opencode.example/v1")
	chat := Conversation{Provider: OpenCodeGo, Format: FormatChat, Messages: []json.RawMessage{json.RawMessage(`{}`)}}
	messages := Conversation{Provider: OpenCodeGo, Messages: chat.Messages}
	kimi, glm, minimax, qwen := Model{OpenCodeGo, "kimi-k3"}, Model{OpenCodeGo, "glm-5.3"}, Model{OpenCodeGo, "minimax-m3"}, Model{OpenCodeGo, "qwen3.8-max"}
	if !chat.Fits(kimi) || !chat.Fits(glm) || chat.Fits(minimax) {
		t.Fatal("a Chat Completions transcript fits only Chat Completions models")
	}
	if !messages.Fits(minimax) || !messages.Fits(qwen) || messages.Fits(kimi) {
		t.Fatal("a Messages transcript fits only Messages models")
	}
}

func TestOpenCodeGoModelsNeedTheirPrefix(t *testing.T) {
	openCodeTest(t, "https://opencode.example/v1")
	// The picker's list must not claim bare model names that belong to DeepSeek or
	// Anthropic.
	for raw, want := range map[string]Model{
		"deepseek-v4-pro":             {DeepSeek, "deepseek-v4-pro"},
		"claude-haiku-5-5":            {Anthropic, "claude-haiku-5-5"},
		"opencode-go:kimi-k3":         {OpenCodeGo, "kimi-k3"},
		"opencode-go:deepseek-v4-pro": {OpenCodeGo, "deepseek-v4-pro"},
	} {
		got, err := ResolveModel(raw)
		if err != nil || got != want {
			t.Errorf("%s: got %v, %v; want %v", raw, got, err, want)
		}
	}
	if _, err := ResolveModel("kimi-k3"); err == nil {
		t.Error("a bare kimi-k3 does not say its provider")
	}
}

func TestOpenCodeGoPickerAndLimits(t *testing.T) {
	openCodeTest(t, "https://opencode.example/v1")
	var offered []string
	for _, model := range ModelChoices(Model{}, map[string][]string{OpenCodeGo: {"kimi-k3", "gpt-6-luna", "omen-alpha"}}) {
		if model.Provider == OpenCodeGo {
			offered = append(offered, model.ID)
		}
	}
	// goat's list first, then what else the server reports, as /model list shows it.
	if want := append(slices.Clone(openCodeModels), "omen-alpha"); !slices.Equal(offered, want) {
		t.Fatalf("got %v", offered)
	}
	for _, model := range openCodeModels {
		if openCodeAPI(model) == apiResponses {
			t.Errorf("the picker offers %s, which goat cannot use", model)
		}
	}
	for model, want := range map[string]int64{"kimi-k3": 1_048_576, "glm-5.3": 1_000_000, "hy3": 256_000, "claude-haiku-5-5": 1_000_000, "omen-alpha": 0} {
		if got := KnownLimitsFor(OpenCodeGo, model).ContextWindow; got != want {
			t.Errorf("%s: window %d, want %d", model, got, want)
		}
	}
	if KnownLimitsFor(OpenCodeGo, "claude-haiku-5-5").PriceStep == 0 {
		t.Error("claude-haiku-5-5 keeps its price step")
	}
}
