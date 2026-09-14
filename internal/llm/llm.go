// Package llm executes queued LLM tasks against an OpenAI-compatible
// chat completions API. Endpoint, key, and default model come from
// config.json in the forebay home directory; each task carries its own
// prompts and optional JSON schema for structured output.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Errors reported by this package. Callers can test for them with errors.Is.
var (
	// ErrConfigMissing reports that no LLM config file was found in the
	// forebay home directory.
	ErrConfigMissing = errors.New("llm: config file missing")

	// ErrConfigInvalid reports that the LLM config file exists but is unusable.
	ErrConfigInvalid = errors.New("llm: invalid config")

	// ErrNoModel reports that neither the task nor the config named a model.
	ErrNoModel = errors.New("llm: no model configured")

	// ErrSpecInvalid reports a task payload that cannot be decoded into a Spec.
	ErrSpecInvalid = errors.New("llm: invalid task payload")

	// ErrBadStatus reports a non-2xx reply from the chat completions endpoint.
	ErrBadStatus = errors.New("llm: endpoint returned an error status")
)

// Config holds API credentials and default settings.
type Config struct {
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key,omitempty"`
	Model          string `json:"model,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	// ExtraBody is merged into every request body, for parameters forebay
	// does not model itself. It cannot set the keys in managedKeys.
	ExtraBody map[string]json.RawMessage `json:"extra_body,omitempty"`
}

const configExample = `{
  "base_url": "https://api.openai.com/v1",
  "api_key": "sk-...",
  "model": "gpt-4o-mini"
}`

// managedKeys are request body fields forebay derives from each task.
var managedKeys = map[string]bool{
	"model": true, "messages": true, "response_format": true,
}

// ConfigPath returns the config file location under the forebay home.
func ConfigPath(homeDir string) string {
	return filepath.Join(homeDir, "config.json")
}

// StripBOM removes a UTF-8 byte order mark.
func StripBOM(data []byte) []byte {
	const bom = "\xEF\xBB\xBF"
	return bytes.TrimPrefix(data, []byte(bom))
}

// LoadConfig reads and validates config.json from the forebay home. If the
// file does not exist, LoadConfig returns an error wrapping ErrConfigMissing;
// if it exists but is unusable, the error wraps ErrConfigInvalid.
func LoadConfig(homeDir string) (Config, error) {
	path := ConfigPath(homeDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, fmt.Errorf("%w: LLM tasks require %s; create it like:\n%s",
				ErrConfigMissing, path, configExample)
		}
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(StripBOM(data), &cfg); err != nil {
		return Config{}, fmt.Errorf("%w: parse %s: %v", ErrConfigInvalid, path, err)
	}
	if cfg.BaseURL == "" {
		return Config{}, fmt.Errorf("%w: %s: base_url is required", ErrConfigInvalid, path)
	}
	for k := range cfg.ExtraBody {
		if managedKeys[k] {
			return Config{}, fmt.Errorf("%w: %s: extra_body cannot set %q; forebay sets it per task",
				ErrConfigInvalid, path, k)
		}
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

// ParseSpec decodes a task payload. If the payload is not valid JSON or has
// no user prompt, the returned error wraps ErrSpecInvalid.
func ParseSpec(payload string) (Spec, error) {
	var s Spec
	if err := json.Unmarshal([]byte(payload), &s); err != nil {
		return Spec{}, fmt.Errorf("%w: corrupt llm payload: %v", ErrSpecInvalid, err)
	}
	if s.User == "" {
		return Spec{}, fmt.Errorf("%w: llm payload has no user prompt", ErrSpecInvalid)
	}
	return s, nil
}

// Exchange is the raw HTTP conversation for one call, written to the
// task log so what was sent and returned can be inspected.
type Exchange struct {
	Request  []byte
	Response []byte
}

// Call performs one chat completion and returns the assistant message content
// plus the raw exchange (for the task log). Cancellation of ctx interrupts the
// request, which is how running LLM tasks get canceled.
func Call(ctx context.Context, cfg Config, spec Spec) (content string, ex Exchange, err error) {
	model := spec.Model
	if model == "" {
		model = cfg.Model
	}
	if model == "" {
		return "", ex, fmt.Errorf("%w: set \"model\" in config.json or pass one on the task", ErrNoModel)
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
	for k, v := range cfg.ExtraBody {
		if _, managed := body[k]; managed {
			continue
		}
		body[k] = v
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return "", ex, fmt.Errorf("encode request for %s: %w", urlChatCompletions(cfg.BaseURL), err)
	}
	ex.Request = reqBody

	url := urlChatCompletions(cfg.BaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", ex, fmt.Errorf("build request for %s: %w", url, err)
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
		return "", ex, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	ex.Response = raw
	if readErr != nil {
		return "", ex, fmt.Errorf("read response from %s: %w", url, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", ex, fmt.Errorf("%w: POST %s: %s: %s", ErrBadStatus, url, resp.Status,
			truncate(string(raw), maxErrorBody))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", ex, fmt.Errorf("unexpected response shape from %s: %w", url, err)
	}
	if len(parsed.Choices) == 0 {
		return "", ex, fmt.Errorf("response from %s has no choices", url)
	}
	return parsed.Choices[0].Message.Content, ex, nil
}

// maxErrorBody caps how much of an error response body is quoted back.
const maxErrorBody = 500

// urlChatCompletions returns the chat completions endpoint for a base URL.
func urlChatCompletions(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/chat/completions"
}

// truncate shortens s to at most n characters, appending "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
