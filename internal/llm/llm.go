// Package llm executes queued LLM tasks against an OpenAI-compatible
// chat completions API. Endpoint, key, and default model come from
// config.json in the forebay home directory; each task carries its own
// prompts and optional JSON schema for structured output.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config holds API credentials and default settings.
type Config struct {
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key,omitempty"`
	Model          string `json:"model,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

const configExample = `{
  "base_url": "https://api.openai.com/v1",
  "api_key": "sk-...",
  "model": "gpt-4o-mini"
}`

// ConfigPath returns the config file location under the forebay home.
func ConfigPath(homeDir string) string {
	return filepath.Join(homeDir, "config.json")
}

// StripBOM removes a UTF-8 byte order mark.
func StripBOM(data []byte) []byte {
	const bom = "\xEF\xBB\xBF"
	return bytes.TrimPrefix(data, []byte(bom))
}

// LoadConfig reads and validates config.json from the forebay home.
func LoadConfig(homeDir string) (Config, error) {
	path := ConfigPath(homeDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, fmt.Errorf("LLM tasks require %s; create it like:\n%s", path, configExample)
		}
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(StripBOM(data), &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.BaseURL == "" {
		return Config{}, fmt.Errorf("%s: base_url is required", path)
	}
	return cfg, nil
}

// Spec is one queued LLM call, stored as a task's JSON payload.
type Spec struct {
	Model  string          `json:"model,omitempty"` // falls back to config model
	System string          `json:"system,omitempty"`
	User   string          `json:"user"`
	Schema json.RawMessage `json:"schema,omitempty"` // JSON schema for structured output
}

// ParseSpec decodes a task payload.
func ParseSpec(payload string) (Spec, error) {
	var s Spec
	if err := json.Unmarshal([]byte(payload), &s); err != nil {
		return Spec{}, fmt.Errorf("corrupt llm payload: %w", err)
	}
	if s.User == "" {
		return Spec{}, fmt.Errorf("llm payload has no user prompt")
	}
	return s, nil
}

// Call performs one chat completion and returns the assistant message
// content plus the raw response body (for the task log). It honors ctx
// cancellation, which is how running LLM tasks get canceled.
func Call(ctx context.Context, cfg Config, spec Spec) (content string, raw []byte, err error) {
	model := spec.Model
	if model == "" {
		model = cfg.Model
	}
	if model == "" {
		return "", nil, fmt.Errorf("no model: set \"model\" in config.json or pass one on the task")
	}

	var messages []map[string]string
	if spec.System != "" {
		messages = append(messages, map[string]string{"role": "system", "content": spec.System})
	}
	messages = append(messages, map[string]string{"role": "user", "content": spec.User})

	body := map[string]any{"model": model, "messages": messages}
	if len(spec.Schema) > 0 {
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "result",
				"strict": true,
				"schema": json.RawMessage(spec.Schema),
			},
		}
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return "", nil, err
	}

	url := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", raw, fmt.Errorf("read response from %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", raw, fmt.Errorf("POST %s: %s: %s", url, resp.Status, truncate(string(raw), 500))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", raw, fmt.Errorf("unexpected response shape from %s: %w", url, err)
	}
	if len(parsed.Choices) == 0 {
		return "", raw, fmt.Errorf("response from %s has no choices", url)
	}
	return parsed.Choices[0].Message.Content, raw, nil
}

// truncate shortens s to at most n characters, appending "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
