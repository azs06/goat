package provider

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// BaseURLVars name the variable that moves each provider's endpoint, for a proxy or a
// compatible API.
var BaseURLVars = map[string]string{
	OpenAI:    "OPENAI_BASE_URL",
	Anthropic: "ANTHROPIC_BASE_URL",
	DeepSeek:  "DEEPSEEK_BASE_URL",
}

// officialEndpoints are the providers' own APIs, used when no base URL is set.
var officialEndpoints = map[string]string{
	OpenAI:    "https://api.openai.com/v1",
	Anthropic: "https://api.anthropic.com",
	DeepSeek:  defaultDeepSeekBaseURL,
}

// Endpoint is where a provider's requests, and with them its key, are sent.
type Endpoint struct {
	URL string
	// Custom is set when URL is not the provider's own API, such as a proxy.
	Custom bool
	// Unencrypted is set when the key would cross the network in plain text: http to a
	// host other than this machine.
	Unencrypted bool
}

// Origin is the endpoint's scheme and host, which is all goat shows of it: some proxies
// take a token in the path or query.
func (e Endpoint) Origin() string {
	u, err := url.Parse(e.URL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// ResolveEndpoint reads the provider's base URL variable and checks it. A URL must use
// http or https and must not carry a user name or password. Errors name the variable but
// not its value. An entry's URL was checked when its file was read.
func ResolveEndpoint(provider string) (Endpoint, error) {
	if entry, ok := entries[provider]; ok {
		return entry.endpoint, nil
	}
	official := officialEndpoints[provider]
	name := BaseURLVars[provider]
	raw := strings.TrimRight(strings.TrimSpace(os.Getenv(name)), "/")
	if raw == "" {
		return Endpoint{URL: official}, nil
	}
	return checkBaseURL(name, raw, official)
}

// checkBaseURL checks a base URL set under label, against the provider's official API
// (none for an entry, which is always custom).
func checkBaseURL(label, raw, official string) (Endpoint, error) {
	raw = strings.TrimRight(raw, "/")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return Endpoint{}, fmt.Errorf("%s is not a URL such as https://proxy.example/v1", label)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return Endpoint{}, fmt.Errorf("%s must start with https:// (or http://)", label)
	}
	if u.User != nil {
		return Endpoint{}, fmt.Errorf("%s must not contain a user name or password", label)
	}
	custom := true
	if officialURL, err := url.Parse(official); official != "" && err == nil {
		custom = scheme != "https" || !strings.EqualFold(u.Hostname(), officialURL.Hostname()) || (u.Port() != "" && u.Port() != "443")
	}
	return Endpoint{
		URL:         raw,
		Custom:      custom,
		Unencrypted: scheme == "http" && !isLoopback(u.Hostname()),
	}, nil
}

// endpointOrOfficial is the provider's endpoint, or its official API when the base URL
// is invalid. Providers are only created once Unavailable has checked the URL, and the
// official API is the one place the key is always meant for.
func endpointOrOfficial(provider string) Endpoint {
	endpoint, err := ResolveEndpoint(provider)
	if err != nil {
		return Endpoint{URL: officialEndpoints[provider]}
	}
	return endpoint
}

// isLoopback reports hosts that are always this machine: localhost and loopback
// addresses. A name such as llm.localhost is looked up like any other, so it does not
// count.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// keyClient is the HTTP client for requests that carry a key. It never follows a
// redirect: the SDKs send the key in headers such as x-api-key, which Go copies to
// whatever host a redirect names, so a redirect could carry it somewhere goat never
// checked. The redirect comes back as an error response instead. It also never sends a
// plain http request through HTTP_PROXY, where the proxy would read the key; https goes
// through a proxy as a tunnel it cannot read. headerTimeout bounds the wait for a
// response to start; zero means no bound.
func keyClient(headerTimeout time.Duration) *http.Client {
	transport := &http.Transport{}
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = t.Clone()
	}
	transport.Proxy = proxyOnlyEncrypted
	transport.ResponseHeaderTimeout = headerTimeout
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func proxyOnlyEncrypted(r *http.Request) (*url.URL, error) {
	if r.URL.Scheme != "https" {
		return nil, nil
	}
	return http.ProxyFromEnvironment(r)
}

// HideEndpointPaths shortens every base URL the user set, in a variable or an entry, to
// its scheme and host, so a token in its path or query does not reach the screen, the
// history or a compaction summary through an error that quotes the request URL. Request URLs keep the base URL's
// path but not its query, so that form is shortened too.
func HideEndpointPaths(text string) string {
	for _, name := range Names {
		_, isEntry := entries[name]
		if !isEntry && strings.TrimSpace(os.Getenv(BaseURLVars[name])) == "" {
			continue
		}
		endpoint, err := ResolveEndpoint(name)
		if err != nil {
			continue
		}
		origin := endpoint.Origin()
		forms := []string{endpoint.URL}
		if u, err := url.Parse(endpoint.URL); err == nil {
			forms = append(forms, origin+strings.TrimRight(u.EscapedPath(), "/"))
		}
		for _, form := range forms {
			if len(form) > len(origin) {
				text = replaceFold(text, form, origin+"/…")
			}
		}
	}
	return text
}

// replaceFold replaces every match of old in s, ignoring case, since a URL's scheme and
// host may be written in any case.
func replaceFold(s, old, replacement string) string {
	var b strings.Builder
	lower, oldLower := strings.ToLower(s), strings.ToLower(old)
	for {
		i := strings.Index(lower, oldLower)
		if i < 0 || len(lower) != len(s) {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(replacement)
		s, lower = s[i+len(old):], lower[i+len(old):]
	}
}

// CheckBaseURL checks a base URL the way ResolveEndpoint does, naming it by label in
// errors and never repeating it.
func CheckBaseURL(label, raw string) error {
	_, err := checkBaseURL(label, strings.TrimSpace(raw), "")
	return err
}
