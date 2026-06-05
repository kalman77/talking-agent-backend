// Package dicta provides a client for the Dicta Nakdan service, which
// adds niqqud (Hebrew vowel marks) to unvocalized Hebrew text.
//
// We use it as a TTS preprocessor: ElevenLabs (and most TTS engines)
// pronounce Hebrew far more accurately when the text carries niqqud,
// because Hebrew without vowel marks is highly ambiguous.
//
// Dicta exposes a free public endpoint; no API key required.
package dicta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/kalman77/aria/internal/application"
)

// Nakdan implements application.TextPreprocessor by calling the Dicta
// Nakdan HTTP API. Only Hebrew-containing text is sent; other text is
// returned unchanged so non-Hebrew sentences skip the network round-trip.
var _ application.TextPreprocessor = (*Nakdan)(nil)

// Default endpoint. Dicta hosts the niqqud service publicly; this
// matches the address advertised on https://nakdanpro.dicta.org.il/ .
const defaultEndpoint = "https://nakdan-5-1.loadbalancer.dicta.org.il/api"

type Nakdan struct {
	endpoint string
	client   *http.Client
	logger   application.Logger
}

type Config struct {
	// Endpoint overrides the default Dicta endpoint. Optional.
	Endpoint string
	// Timeout caps each request. Defaults to 4s — niqqud must not
	// dominate first-audio latency.
	Timeout time.Duration
	// Logger is used to log non-fatal failures. Optional.
	Logger application.Logger
}

func New(cfg Config) *Nakdan {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 4 * time.Second
	}
	return &Nakdan{
		endpoint: endpoint,
		client:   &http.Client{Timeout: timeout},
		logger:   cfg.Logger,
	}
}

// Process returns the input text vocalized with niqqud. If the text
// contains no Hebrew, it is returned unchanged with no network call.
// Errors are surfaced to the caller; the turn pipeline treats them as
// non-fatal and falls back to the original text.
func (n *Nakdan) Process(ctx context.Context, text string) (string, error) {
	if !containsHebrew(text) {
		return text, nil
	}

	reqBody := struct {
		Task  string `json:"task"`
		Data  string `json:"data"`
		Genre string `json:"genre"`
	}{
		Task:  "nakdan",
		Data:  text,
		Genre: "modern",
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return text, fmt.Errorf("nakdan marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.endpoint, bytes.NewReader(body))
	if err != nil {
		return text, fmt.Errorf("nakdan request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return text, fmt.Errorf("nakdan do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return text, fmt.Errorf("nakdan status %d", resp.StatusCode)
	}

	// Dicta Nakdan returns a JSON array of tokens. Each token is either a
	// separator (sep=true, `word` holds the literal whitespace/punctuation)
	// or a real word (sep=false, `options[0]` holds the best vocalized form).
	type token struct {
		Word    string   `json:"word"`
		Sep     bool     `json:"sep"`
		Options []string `json:"options"`
	}
	var tokens []token
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return text, fmt.Errorf("nakdan decode: %w", err)
	}

	var out strings.Builder
	out.Grow(len(text) * 2)
	for _, t := range tokens {
		if t.Sep {
			out.WriteString(t.Word)
			continue
		}
		if len(t.Options) > 0 && t.Options[0] != "" {
			// Dicta uses '|' as a morpheme-boundary marker inside the
			// vocalized form. Strip it — TTS just needs the clean word.
			out.WriteString(strings.ReplaceAll(t.Options[0], "|", ""))
		} else {
			// No options — fall back to the original word.
			out.WriteString(t.Word)
		}
	}

	result := out.String()
	if result == "" {
		// Defensive: if the response shape didn't match what we expect,
		// don't return an empty string and break TTS — fall back.
		return text, nil
	}
	return result, nil
}

// containsHebrew returns true if any rune in s is in the Hebrew Unicode
// block. Cheap pre-filter so we don't ping Dicta for English-only text.
func containsHebrew(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Hebrew, r) {
			return true
		}
	}
	return false
}
