// Package anthropic implements application.LLM using Claude.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kalman77/aria/internal/application"
	"github.com/kalman77/aria/internal/domain"
)

const endpoint = "https://api.anthropic.com/v1/messages"
const apiVersion = "2023-06-01"

// Claude implements application.LLM.
//
// Compile-time assertion: this must satisfy the LLM interface. If we ever
// drift, the build breaks here and points us at the discrepancy.
var _ application.LLM = (*Claude)(nil)

// Claude is the Anthropic provider. Stateless beyond config; safe for
// concurrent use across multiple ConversationServices.
type Claude struct {
	apiKey    string
	model     string
	maxTokens int
	client    *http.Client
}

// Config bundles construction parameters for clarity.
type Config struct {
	APIKey    string
	Model     string // e.g. "claude-sonnet-4-6"
	MaxTokens int    // 0 → defaults to 1024
}

// New constructs a Claude provider.
func New(cfg Config) *Claude {
	max := cfg.MaxTokens
	if max == 0 {
		max = 1024
	}
	return &Claude{
		apiKey:    cfg.APIKey,
		model:     cfg.Model,
		maxTokens: max,
		// No timeout: streaming responses can take many seconds. Context
		// cancellation handles abort.
		client: &http.Client{},
	}
}

// Stream implements application.LLM.
func (c *Claude) Stream(
	ctx context.Context,
	system string,
	history domain.ChatHistory,
	onDelta func(string),
) error {
	body, err := json.Marshal(buildRequest(c.model, c.maxTokens, system, history))
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("anthropic-version", apiVersion)
	req.Header.Set("x-api-key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("claude http %d: %s", resp.StatusCode, string(errBody))
	}

	return parseSSE(ctx, resp.Body, onDelta)
}

// ─── private helpers ───

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type request struct {
	Model     string    `json:"model"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	MaxTokens int       `json:"max_tokens"`
	Stream    bool      `json:"stream"`
}

func buildRequest(model string, maxTokens int, system string, history domain.ChatHistory) request {
	msgs := make([]message, 0, len(history))
	for _, h := range history {
		msgs = append(msgs, message{Role: string(h.Role), Content: h.Content})
	}
	return request{
		Model:     model,
		System:    system, // privileged instruction layer (defense layer 3)
		Messages:  msgs,
		MaxTokens: maxTokens,
		Stream:    true,
	}
}

type sseDelta struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

type sseError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// parseSSE reads the SSE stream and invokes onDelta for each text fragment.
//
// Anthropic SSE format:
//
//	event: <name>\n
//	data: <json>\n
//	\n
func parseSSE(ctx context.Context, body io.Reader, onDelta func(string)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var currentEvent string
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			currentEvent = strings.TrimSpace(line[len("event: "):])
		case strings.HasPrefix(line, "data: "):
			payload := line[len("data: "):]
			if currentEvent == "error" {
				var e sseError
				if err := json.Unmarshal([]byte(payload), &e); err == nil {
					return fmt.Errorf("claude stream error: %s: %s", e.Error.Type, e.Error.Message)
				}
				return fmt.Errorf("claude stream error: %s", payload)
			}
			if currentEvent != "content_block_delta" {
				continue
			}
			var ev sseDelta
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				continue // forward-compatible: unknown subtype, skip
			}
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				onDelta(ev.Delta.Text)
			}
		case line == "":
			currentEvent = ""
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("scan stream: %w", err)
	}
	return nil
}
