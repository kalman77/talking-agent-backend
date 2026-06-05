package elevenlabs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"unicode"

	"github.com/coder/websocket"

	"github.com/kalman77/aria/internal/application"
)

// TTS implements application.TTS using ElevenLabs WebSocket TTS.
var _ application.TTS = (*TTS)(nil)

// ttsSampleRate is the sample rate of the audio ElevenLabs returns.
// Must match the output_format query parameter sent on the WebSocket.
// 24 kHz is the highest rate available below the ElevenLabs Pro tier
// and still preserves the full speech bandwidth (~12 kHz), so the voice
// sounds the same as in the web playground.
const ttsSampleRate = 24000

type TTS struct {
	apiKey   string
	voiceID  string            // default / fallback voice
	voiceIDs map[string]string // ISO lang code → voice id (e.g. {"he": "..."} )
	model    string            // e.g. "eleven_multilingual_v2"
	logger   application.Logger
}

type TTSConfig struct {
	APIKey   string
	VoiceID  string            // default / fallback voice id
	VoiceIDs map[string]string // optional per-language overrides, keyed by ISO code
	Model    string
	Logger   application.Logger // optional
}

func NewTTS(cfg TTSConfig) *TTS {
	model := cfg.Model
	if model == "" {
		model = "eleven_multilingual_v2"
	}
	voiceID := cfg.VoiceID
	// When no single default voice is configured, fall back to one from the
	// per-language map so unlisted languages still get a valid voice instead
	// of an empty id (which would produce a malformed request URL). Prefer
	// English, otherwise take any entry.
	if voiceID == "" && len(cfg.VoiceIDs) > 0 {
		if en, ok := cfg.VoiceIDs["en"]; ok {
			voiceID = en
		} else {
			for _, id := range cfg.VoiceIDs {
				voiceID = id
				break
			}
		}
	}
	return &TTS{
		apiKey:   cfg.APIKey,
		voiceID:  voiceID,
		voiceIDs: cfg.VoiceIDs,
		model:    model,
		logger:   cfg.Logger,
	}
}

// pickVoice returns the voice id appropriate for the dominant script in
// text. If no language-specific voice is configured for the detected
// language, the default voiceID is used.
func (t *TTS) pickVoice(text string) string {
	if len(t.voiceIDs) == 0 {
		return t.voiceID
	}
	if id, ok := t.voiceIDs[detectLang(text)]; ok && id != "" {
		return id
	}
	return t.voiceID
}

// detectLang returns a coarse ISO language code based on the dominant
// Unicode script in text. Counts letter runes per script and picks the
// max; ties resolve to the first script seen. Returns "en" when no
// scripted letters are present (numbers, punctuation, English).
//
// This is deliberately script-based, not statistical: per sentence it's
// fast, deterministic, and good enough to route to the right voice.
func detectLang(text string) string {
	counts := map[string]int{}
	for _, r := range text {
		if !unicode.IsLetter(r) {
			continue
		}
		switch {
		case unicode.Is(unicode.Hebrew, r):
			counts["he"]++
		case unicode.Is(unicode.Arabic, r):
			counts["ar"]++
		case unicode.Is(unicode.Cyrillic, r):
			counts["ru"]++
		case unicode.Is(unicode.Han, r):
			counts["zh"]++
		case unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r):
			counts["ja"]++
		case unicode.Is(unicode.Hangul, r):
			counts["ko"]++
		case unicode.Is(unicode.Greek, r):
			counts["el"]++
		default:
			counts["en"]++ // Latin and anything else falls here
		}
	}
	best, bestN := "en", 0
	for lang, n := range counts {
		if n > bestN {
			best, bestN = lang, n
		}
	}
	return best
}

// Synthesize implements application.TTS.
func (t *TTS) Synthesize(
	ctx context.Context,
	text string,
	onAudio func(application.AudioChunk),
) error {
	voiceID := t.pickVoice(text)
	if t.logger != nil {
		t.logger.Info("tts voice",
			"voiceID", voiceID,
			"detectedLang", detectLang(text),
			"model", t.model,
			"text", text,
		)
	}
	u := url.URL{
		Scheme: "wss",
		Host:   "api.elevenlabs.io",
		Path:   fmt.Sprintf("/v1/text-to-speech/%s/stream-input", voiceID),
		RawQuery: url.Values{
			"model_id":      {t.model},
			"output_format": {"pcm_24000"},
			"auto_mode":     {"true"},
		}.Encode(),
	}

	conn, _, err := websocket.Dial(ctx, u.String(), nil)
	if err != nil {
		return fmt.Errorf("tts dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")
	conn.SetReadLimit(2 << 20)

	// BOS — auth only. We deliberately omit voice_settings so ElevenLabs
	// uses each voice's library defaults; the voice author tunes these
	// per voice, and overriding them tends to produce the "wrong accent"
	// effect users hear vs. the website playground.
	type bos struct {
		Text     string `json:"text"`
		XIAPIKey string `json:"xi_api_key"`
	}
	if err := writeJSON(ctx, conn, bos{
		Text:     " ",
		XIAPIKey: t.apiKey,
	}); err != nil {
		return fmt.Errorf("tts bos: %w", err)
	}

	// Sentence with flush — generate immediately, don't buffer.
	type textMsg struct {
		Text  string `json:"text"`
		Flush bool   `json:"flush,omitempty"`
	}
	if err := writeJSON(ctx, conn, textMsg{Text: text + " ", Flush: true}); err != nil {
		return fmt.Errorf("tts text: %w", err)
	}

	// EOS.
	if err := writeJSON(ctx, conn, textMsg{Text: ""}); err != nil {
		return fmt.Errorf("tts eos: %w", err)
	}

	// Read audio chunks until isFinal or ctx cancellation.
	type response struct {
		Audio   string `json:"audio"`
		IsFinal bool   `json:"isFinal"`
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("tts read: %w", err)
		}
		var resp response
		if err := json.Unmarshal(data, &resp); err != nil {
			continue
		}
		if resp.Audio != "" {
			pcm, err := base64.StdEncoding.DecodeString(resp.Audio)
			if err != nil {
				return fmt.Errorf("tts decode: %w", err)
			}
			if len(pcm) > 0 {
				onAudio(application.AudioChunk{
					PCM:        pcm,
					SampleRate: ttsSampleRate,
				})
			}
		}
		if resp.IsFinal {
			return nil
		}
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}
