// Package llm asks a model to propose query rewrites.
//
// The model's output is never trusted and never executed against user data on
// its say-so. It proposes; internal/optimizer verifies. That separation is the
// whole point of the service, and it is why this package is deliberately small:
// it returns text, and everything interesting happens to that text elsewhere.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const endpoint = "https://generativelanguage.googleapis.com/v1beta"

type Client struct {
	apiKey      string
	model       string
	http        *http.Client
	maxRetries  int
	retryBudget time.Duration
}

func New(apiKey, model string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 90 * time.Second
	}
	return &Client{
		apiKey: apiKey, model: model,
		http:        &http.Client{Timeout: timeout},
		maxRetries:  5,
		retryBudget: 90 * time.Second,
	}
}

func (c *Client) Model() string { return c.model }
func (c *Client) Enabled() bool { return c != nil && c.apiKey != "" }

type Part struct {
	Text             string `json:"text,omitempty"`
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
	Thought          bool   `json:"thought,omitempty"`
}

type Content struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts"`
}

type GenerationConfig struct {
	Temperature      float64        `json:"temperature,omitempty"`
	MaxOutputTokens  int            `json:"maxOutputTokens,omitempty"`
	ResponseMIMEType string         `json:"responseMimeType,omitempty"`
	ResponseSchema   map[string]any `json:"responseSchema,omitempty"`
}

type Usage struct {
	PromptTokens int `json:"promptTokenCount"`
	OutputTokens int `json:"candidatesTokenCount"`
	TotalTokens  int `json:"totalTokenCount"`
}

type Request struct {
	System string
	Prompt string
	Config GenerationConfig
}

type Response struct {
	Text    string
	Usage   Usage
	Latency time.Duration
}

type apiRequest struct {
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	Contents          []Content         `json:"contents"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
}

type apiResponse struct {
	Candidates []struct {
		Content      Content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata  Usage `json:"usageMetadata"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback,omitempty"`
}

func (c *Client) Generate(ctx context.Context, req Request) (*Response, error) {
	if !c.Enabled() {
		return nil, errors.New("GEMINI_API_KEY is not set")
	}
	body := apiRequest{
		Contents: []Content{{Role: "user", Parts: []Part{{Text: req.Prompt}}}},
	}
	if req.System != "" {
		body.SystemInstruction = &Content{Parts: []Part{{Text: req.System}}}
	}
	cfg := req.Config
	body.GenerationConfig = &cfg

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", endpoint, c.model, c.apiKey)

	start := time.Now()
	var lastErr error
	deadline := time.Now().Add(c.retryBudget)

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("retry budget exhausted: %w", lastErr)
			}
			wait := time.Duration(1<<attempt) * time.Second
			if wait > 20*time.Second {
				wait = 20 * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(httpReq)
		if err != nil {
			// The URL carries the API key and net/http puts it in the error.
			lastErr = c.redact(err)
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("gemini %d: %s", resp.StatusCode, truncate(string(raw), 200))
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("gemini %d: %s", resp.StatusCode, truncate(string(raw), 400))
		}

		var ar apiResponse
		if err := json.Unmarshal(raw, &ar); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if len(ar.Candidates) == 0 {
			reason := ""
			if ar.PromptFeedback != nil {
				reason = ar.PromptFeedback.BlockReason
			}
			return nil, fmt.Errorf("model returned no candidates (block reason %q)", reason)
		}
		var sb strings.Builder
		for _, p := range ar.Candidates[0].Content.Parts {
			if !p.Thought {
				sb.WriteString(p.Text)
			}
		}
		return &Response{Text: sb.String(), Usage: ar.UsageMetadata, Latency: time.Since(start)}, nil
	}
	return nil, fmt.Errorf("request failed after %d attempts: %w", c.maxRetries+1, lastErr)
}

func (c *Client) redact(err error) error {
	if err == nil || c.apiKey == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, c.apiKey) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, c.apiKey, "REDACTED"))
}

// ExtractJSON pulls the first JSON value out of a response that may be wrapped
// in prose or a code fence.
func ExtractJSON(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if fence := strings.Index(s, "```"); fence >= 0 {
		rest := s[fence+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			s = strings.TrimSpace(rest[:end])
		}
	}
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return "", false
	}
	open := s[start]
	closeCh := byte('}')
	if open == '[' {
		closeCh = ']'
	}
	depth, inStr, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inStr:
			escaped = true
		case ch == '"':
			inStr = !inStr
		case inStr:
		case ch == open:
			depth++
		case ch == closeCh:
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
