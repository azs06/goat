package provider

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
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
// not its value.
func ResolveEndpoint(provider string) (Endpoint, error) {
	official := officialEndpoints[provider]
	name := BaseURLVars[provider]
	raw := strings.TrimRight(strings.TrimSpace(os.Getenv(name)), "/")
	if raw == "" {
		return Endpoint{URL: official}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return Endpoint{}, fmt.Errorf("%s is not a URL such as https://proxy.example/v1", name)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return Endpoint{}, fmt.Errorf("%s must start with https://", name)
	}
	if u.User != nil {
		return Endpoint{}, fmt.Errorf("%s must not contain a user name or password", name)
	}
	officialURL, _ := url.Parse(official)
	return Endpoint{
		URL:         raw,
		Custom:      scheme != "https" || !strings.EqualFold(u.Hostname(), officialURL.Hostname()) || (u.Port() != "" && u.Port() != "443"),
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

func isLoopback(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
