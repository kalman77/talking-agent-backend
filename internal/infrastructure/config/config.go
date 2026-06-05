// Package config loads runtime configuration from environment variables.
//
// Belongs in infrastructure (not domain or application) because env
// variables are an OS concern, and which provider we configure is
// determined here.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port              string
	AllowedOrigin     string
	MaxSessionSeconds int
	SystemPromptPath  string

	AnthropicAPIKey string
	AnthropicModel  string

	ElevenLabsAPIKey   string
	ElevenLabsVoiceID  string            // default / fallback voice
	ElevenLabsVoiceIDs map[string]string // per-language overrides, keyed by ISO code (he, en, ar, ...)
	ElevenLabsTTSModel string
}

func Load() (Config, error) {
	cfg := Config{
		Port:               envOr("PORT", "8080"),
		AllowedOrigin:      envOr("ALLOWED_ORIGIN", "*"),
		MaxSessionSeconds:  envOrInt("MAX_SESSION_SECONDS", 300),
		SystemPromptPath:   envOr("SYSTEM_PROMPT_PATH", "prompts/persona_aria.md"),
		AnthropicAPIKey:    os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel:     envOr("CLAUDE_MODEL", "claude-sonnet-4-6"),
		ElevenLabsAPIKey:   os.Getenv("ELEVENLABS_API_KEY"),
		ElevenLabsVoiceID:  os.Getenv("ELEVENLABS_VOICE_ID"),
		ElevenLabsVoiceIDs: parseVoiceMap(os.Getenv("ELEVENLABS_VOICE_IDS")),
		ElevenLabsTTSModel: envOr("ELEVENLABS_TTS_MODEL", "eleven_multilingual_v2"),
	}

	for k, v := range map[string]string{
		"ANTHROPIC_API_KEY":  cfg.AnthropicAPIKey,
		"ELEVENLABS_API_KEY": cfg.ElevenLabsAPIKey,
	} {
		if v == "" {
			return Config{}, fmt.Errorf("required env var %s not set", k)
		}
	}
	// A voice is required, but it can come from either the single default
	// (ELEVENLABS_VOICE_ID) or the per-language map (ELEVENLABS_VOICE_IDS).
	if cfg.ElevenLabsVoiceID == "" && len(cfg.ElevenLabsVoiceIDs) == 0 {
		return Config{}, fmt.Errorf("set ELEVENLABS_VOICE_ID or ELEVENLABS_VOICE_IDS")
	}
	return cfg, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// parseVoiceMap parses a string like "he:abc123,ar:def456" into a map
// keyed by ISO language code. Whitespace around entries is tolerated;
// empty or malformed entries are skipped silently so a typo doesn't
// crash startup.
func parseVoiceMap(s string) map[string]string {
	out := map[string]string{}
	if s == "" {
		return out
	}
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		lang, id, ok := strings.Cut(entry, ":")
		if !ok {
			continue
		}
		lang = strings.TrimSpace(strings.ToLower(lang))
		id = strings.TrimSpace(id)
		if lang == "" || id == "" {
			continue
		}
		out[lang] = id
	}
	return out
}

func envOrInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
