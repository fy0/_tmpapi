// Package basispoints implements the client-facing side of the ChatGPT
// Basispoints (Excel plugin) Responses channel: request-body translation into
// the strict upstream whitelist schema, the Excel client header profile, and
// conversion of the upstream SSE stream back into a standard Responses
// response.
//
// This is a port of the verified bps_proxy.py reference implementation (the
// standalone ChatGPT Basispoints -> Responses API reverse proxy); see the
// account extra keys openai_basispoints* for how it is wired into the OpenAI
// gateway forward path.
package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	Provider = "oai-basispoints"

	DefaultResponsesURL  = "https://bps.openai.com/basispoints/api/responses"
	DefaultUpstreamModel = "gpt-6-astra"
	DefaultAuthMode      = "chatgpt"
)

// Config carries the per-account knobs for the Basispoints channel.
type Config struct {
	ResponsesURL     string
	UpstreamModel    string
	TimeoutSeconds   int
	MaxResponseBytes int64
	AuthMode         string
	ToolsVersionID   string
	// MaxInputTokens 是本地超限拒绝阈值（估算法：input 序列化字节数/3，
	// 对齐参考实现 BPS_MAX_INPUT_TOKENS=300000）。
	MaxInputTokens int
	// Timezone is sent via the x-oai-timezone header (the only channel the
	// upstream accepts for timezone; body/metadata keys are rejected).
	Timezone string
}

// DefaultConfig returns the verified-good defaults.
func DefaultConfig() Config {
	return Config{
		ResponsesURL:     DefaultResponsesURL,
		UpstreamModel:    DefaultUpstreamModel,
		TimeoutSeconds:   300,
		MaxResponseBytes: 64 << 20,
		MaxInputTokens:   DefaultMaxInputTokens,
		AuthMode:         DefaultAuthMode,
	}
}

// Normalize validates and fills defaults. Returns an error for unusable
// operator-supplied values (bad URL / out-of-range limits).
func (c *Config) Normalize() error {
	if c == nil {
		return fmt.Errorf("basispoints config is missing")
	}
	c.ResponsesURL = strings.TrimSpace(c.ResponsesURL)
	if c.ResponsesURL == "" {
		c.ResponsesURL = DefaultResponsesURL
	}
	u, err := url.Parse(c.ResponsesURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("basispoints responses_url must be an absolute HTTP(S) URL")
	}
	c.UpstreamModel = strings.TrimSpace(c.UpstreamModel)
	if c.UpstreamModel == "" {
		c.UpstreamModel = DefaultUpstreamModel
	}
	c.AuthMode = strings.TrimSpace(c.AuthMode)
	if c.AuthMode == "" {
		c.AuthMode = DefaultAuthMode
	}
	c.Timezone = strings.TrimSpace(c.Timezone)
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 1800 {
		return fmt.Errorf("basispoints timeout_seconds must be between 0 and 1800")
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 300
	}
	if c.MaxResponseBytes < 0 || c.MaxResponseBytes > 128<<20 {
		return fmt.Errorf("basispoints max_response_bytes must be between 0 and 128 MiB")
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = 64 << 20
	}
	if c.MaxInputTokens < 0 || c.MaxInputTokens > 10_000_000 {
		return fmt.Errorf("basispoints max_input_tokens must be between 0 and 10000000")
	}
	if c.MaxInputTokens == 0 {
		c.MaxInputTokens = DefaultMaxInputTokens
	}
	return nil
}

// AuthHeaders builds the complete Excel-plugin client profile header set the
// upstream requires, plus authentication. stream toggles the Accept header.
// Timezone (IANA name) is only emitted when configured.
func AuthHeaders(accessToken, accountID string, stream bool, cfg Config) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	h.Set("Accept-Encoding", "identity")
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("Chatgpt-Account-Id", accountID)
	h.Set("X-Openai-Account-Id", accountID)
	authMode := strings.TrimSpace(cfg.AuthMode)
	if authMode == "" {
		authMode = DefaultAuthMode
	}
	h.Set("X-Basispoints-Auth-Mode", authMode)
	origin := DefaultResponsesURL
	if u, err := url.Parse(cfg.ResponsesURL); err == nil && u.Scheme != "" && u.Host != "" {
		origin = u.Scheme + "://" + u.Host
	}
	h.Set("Origin", origin)
	h.Set("X-Openai-Internal-Basispoints-Client-Agent-Profile", "excel")
	h.Set("X-Openai-Internal-Basispoints-Client-Editor", "excel")
	h.Set("X-Openai-Internal-Basispoints-Client-Host", "office")
	h.Set("X-Openai-Internal-Basispoints-Client-Platform", "excel")
	h.Set("X-Openai-Internal-Basispoints-Client-Platform-Class", "PC")
	h.Set("X-Openai-Internal-Basispoints-Client-Product", "basispoints-excel-plugin")
	h.Set("X-Openai-Internal-Basispoints-Client-Runtime", "desktop")
	h.Set("X-Openai-Internal-Basispoints-Office-Host", "Excel")
	h.Set("X-Openai-Internal-Basispoints-Office-Platform", "PC")
	h.Set("X-Stainless-Arch", "unknown")
	h.Set("X-Stainless-Lang", "js")
	h.Set("X-Stainless-Os", "Unknown")
	h.Set("X-Stainless-Package-Version", "6.31.0")
	h.Set("X-Stainless-Retry-Count", "0")
	h.Set("X-Stainless-Runtime", "browser:chrome")
	// 参考实现的 user-agent（实测可过；上游不校验浏览器身份）。
	h.Set("User-Agent", "bps-proxy/0.1")
	if cfg.Timezone != "" {
		h.Set("X-Oai-Timezone", cfg.Timezone)
	}
	return h
}

// AccountIDFromToken extracts the ChatGPT account ID from a Codex OAuth access
// token's JWT claims, used as a fallback when the stored credential does not
// carry chatgpt_account_id.
func AccountIDFromToken(token string) string {
	claims := jwtPayload(token)
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if id := firstString(auth, "chatgpt_account_id", "account_id"); id != "" {
			return id
		}
	}
	return firstString(claims, "chatgpt_account_id", "account_id")
}

func jwtPayload(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return map[string]any{}
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if json.Unmarshal(data, &claims) != nil || claims == nil {
		return map[string]any{}
	}
	return claims
}

func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(object[key]); value != "" {
			return value
		}
	}
	return ""
}
