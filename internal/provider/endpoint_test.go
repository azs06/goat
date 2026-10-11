package provider

import (
	"strings"
	"testing"
)

func TestResolveEndpoint(t *testing.T) {
	for _, test := range []struct {
		raw                 string
		url                 string
		custom, unencrypted bool
		err                 string
	}{
		{raw: "", url: "https://api.anthropic.com"},
		{raw: "https://api.anthropic.com/", url: "https://api.anthropic.com"},
		{raw: "HTTPS://API.ANTHROPIC.COM:443", url: "HTTPS://API.ANTHROPIC.COM:443"},
		{raw: "https://proxy.example/v1", url: "https://proxy.example/v1", custom: true},
		{raw: "https://api.anthropic.com:8443", url: "https://api.anthropic.com:8443", custom: true},
		{raw: "http://api.anthropic.com", url: "http://api.anthropic.com", custom: true, unencrypted: true},
		{raw: "http://127.0.0.1:4000", url: "http://127.0.0.1:4000", custom: true},
		{raw: "http://localhost:11434/v1", url: "http://localhost:11434/v1", custom: true},
		{raw: "http://[::1]:8080", url: "http://[::1]:8080", custom: true},
		{raw: "http://192.168.1.5:11434", url: "http://192.168.1.5:11434", custom: true, unencrypted: true},
		// Go sends only localhost and loopback addresses around HTTP_PROXY.
		{raw: "http://llm.localhost:4000", url: "http://llm.localhost:4000", custom: true, unencrypted: true},
		{raw: "https://user:secret-pass@secret.example", err: "must not contain a user name or password"},
		{raw: "ftp://secret.example", err: "must start with https://"},
		{raw: "secret.example/v1", err: "is not a URL"},
		{raw: "localhost:4000", err: "is not a URL"},
		{raw: "https://secret.example/\x7f", err: "is not a URL"},
	} {
		t.Setenv("ANTHROPIC_BASE_URL", test.raw)
		endpoint, err := ResolveEndpoint(Anthropic)
		if test.err != "" {
			if err == nil || !strings.Contains(err.Error(), test.err) || !strings.Contains(err.Error(), "ANTHROPIC_BASE_URL") {
				t.Errorf("%q: expected an error containing %q, got %v", test.raw, test.err, err)
			} else if strings.Contains(err.Error(), "secret-pass") || strings.Contains(err.Error(), "secret.example") {
				t.Errorf("%q: the error must not repeat the value: %v", test.raw, err)
			}
			continue
		}
		if err != nil || endpoint.URL != test.url || endpoint.Custom != test.custom || endpoint.Unencrypted != test.unencrypted {
			t.Errorf("%q: got %+v %v", test.raw, endpoint, err)
		}
	}
}

func TestEndpointOriginHidesThePathAndQuery(t *testing.T) {
	endpoint := Endpoint{URL: "https://proxy.example:8443/t/sk-token/v1?key=sk-other"}
	if origin := endpoint.Origin(); origin != "https://proxy.example:8443" {
		t.Errorf("got %q", origin)
	}
}

func TestAnInvalidBaseURLMakesAProviderUnavailable(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	t.Setenv("DEEPSEEK_BASE_URL", "https://user:pw@deepseek.example")
	if Available(DeepSeek) {
		t.Fatal("a refused base URL should make the provider unavailable")
	}
	if err := Unavailable(DeepSeek); err == nil || !strings.Contains(err.Error(), "DEEPSEEK_BASE_URL") {
		t.Errorf("got %v", err)
	}
	t.Setenv("DEEPSEEK_BASE_URL", "")
	if err := Unavailable(DeepSeek); err != nil {
		t.Errorf("got %v", err)
	}
}

func TestAnthropicTreatsOnlyOtherHostsAsCustom(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	for raw, custom := range map[string]bool{
		"":                          false,
		"https://api.anthropic.com": false,
		"https://proxy.example":     true,
	} {
		t.Setenv("ANTHROPIC_BASE_URL", raw)
		if p := NewAnthropic().(*anthropicProvider); p.customBaseURL != custom {
			t.Errorf("%q: customBaseURL = %v, want %v", raw, p.customBaseURL, custom)
		}
	}
}

func TestHideEndpointPaths(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:9/TOKEN-PATH/v1/")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("DEEPSEEK_BASE_URL", "HTTPS://Proxy.example/t/ds-token/anthropic")
	got := HideEndpointPaths(`Post "http://127.0.0.1:9/TOKEN-PATH/v1/responses": refused; Post "https://proxy.example/t/ds-token/anthropic/v1/messages": 502; https://api.anthropic.com/v1/messages`)
	if strings.Contains(got, "TOKEN-PATH") || strings.Contains(got, "ds-token") {
		t.Errorf("a path survived: %s", got)
	}
	if !strings.Contains(got, "http://127.0.0.1:9/…/responses") || !strings.Contains(got, "https://api.anthropic.com/v1/messages") {
		t.Errorf("got %s", got)
	}
}
