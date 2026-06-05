// Package elevenlabs implements application.STT and application.TTS using
// ElevenLabs' WebSocket APIs.
package elevenlabs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/coder/websocket"

	"github.com/kalman77/aria/internal/application"
	"github.com/kalman77/aria/internal/domain"
)

const sttModel = "scribe_v2_realtime"

// STT implements application.STT using ElevenLabs Scribe Realtime.
//
// IMPORTANT current API quirks (verified against docs May 2026):
//   - The protocol uses "message_type" as the discriminator field, NOT "type".
//   - Audio chunks carry "audio_base_64", NOT "audio".
//   - Default commit_strategy is "manual" — without commit:true on chunks,
//     the server never produces transcripts. We force "vad" so end-of-
//     utterance is detected automatically by silence.
//   - audio_format must match what we send. We send PCM16 mono at 16 kHz,
//     so we pass audio_format=pcm_16000.
var _ application.STT = (*STT)(nil)

type STT struct {
	apiKey       string
	languageCode string
	logger       application.Logger
}

type STTConfig struct {
	APIKey       string
	LanguageCode string // e.g. "he"; defaults to "he"
	Logger       application.Logger
}

func NewSTT(cfg STTConfig) *STT {
	lc := cfg.LanguageCode
	if lc == "" {
		lc = "he"
	}
	return &STT{
		apiKey:       cfg.APIKey,
		languageCode: lc,
		logger:       cfg.Logger,
	}
}

// Transcribe implements application.STT. Closes `events` on exit (per the
// port contract).
func (s *STT) Transcribe(
	ctx context.Context,
	audioFrames <-chan []byte,
	events chan<- domain.TranscriptEvent,
) error {
	defer close(events)

	u := url.URL{
		Scheme: "wss",
		Host:   "api.elevenlabs.io",
		Path:   "/v1/speech-to-text/realtime",
		RawQuery: url.Values{
			"model_id":         {sttModel},
			"language_code":    {s.languageCode},
			"audio_format":     {"pcm_16000"},
			"commit_strategy":  {"vad"}, // CRITICAL: default is manual; we want auto end-of-utterance
		}.Encode(),
	}

	hdr := http.Header{}
	hdr.Set("xi-api-key", s.apiKey)

	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: hdr,
	})
	if err != nil {
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")
	conn.SetReadLimit(1 << 20)

	// Writer goroutine: pump audio frames up.
	go s.writeAudioLoop(ctx, conn, audioFrames)

	// Reader: parse transcript events.
	return s.readEventsLoop(ctx, conn, events)
}

func (s *STT) writeAudioLoop(ctx context.Context, conn *websocket.Conn, in <-chan []byte) {
	type chunkMsg struct {
		MessageType string `json:"message_type"`
		AudioBase64 string `json:"audio_base_64"`
	}
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-in:
			if !ok {
				return
			}
			b, _ := json.Marshal(chunkMsg{
				MessageType: "input_audio_chunk",
				AudioBase64: base64.StdEncoding.EncodeToString(frame),
			})
			if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("stt write", "err", err)
				}
				return
			}
		}
	}
}

func (s *STT) readEventsLoop(
	ctx context.Context,
	conn *websocket.Conn,
	events chan<- domain.TranscriptEvent,
) error {
	type downMsg struct {
		MessageType string `json:"message_type"`
		Text        string `json:"text,omitempty"`
		// Error payloads share these fields across the various error subtypes.
		Code    string `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
		Reason  string `json:"reason,omitempty"`
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
			return err
		}

		// Useful while debugging. Comment out once stable.
		s.logger.Info("stt raw", "data", string(data))

		var msg downMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.MessageType {
		case "session_started":
			s.logger.Info("stt session started")
		case "partial_transcript":
			if msg.Text != "" {
				select {
				case events <- domain.TranscriptEvent{Kind: domain.TranscriptPartial, Text: msg.Text}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		case "committed_transcript", "committed_transcript_with_timestamps":
			if msg.Text != "" {
				select {
				case events <- domain.TranscriptEvent{Kind: domain.TranscriptFinal, Text: msg.Text}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		default:
			// Anything containing "error" in the message_type — treat as fatal,
			// log full payload, and exit cleanly so the user can reconnect.
			// The various error types are: scribe_error, scribe_auth_error,
			// scribe_quota_exceeded_error, scribe_throttled_error, etc.
			if len(msg.MessageType) > 0 {
				s.logger.Error("stt server message",
					"message_type", msg.MessageType,
					"code", msg.Code,
					"message", msg.Message,
					"reason", msg.Reason,
					"raw", string(data),
				)
				// Errors → exit. Other unrecognized message types → keep listening.
				if isErrorMessageType(msg.MessageType) {
					return nil
				}
			}
		}
	}
}

func isErrorMessageType(t string) bool {
	switch t {
	case "scribe_error",
		"scribe_auth_error",
		"scribe_quota_exceeded_error",
		"scribe_throttled_error",
		"scribe_unaccepted_terms_error",
		"scribe_rate_limited_error",
		"scribe_queue_overflow_error",
		"scribe_resource_exhausted_error",
		"scribe_session_time_limit_exceeded_error",
		"scribe_input_error",
		"scribe_chunk_size_exceeded_error",
		"scribe_insufficient_audio_activity_error",
		"scribe_transcriber_error":
		return true
	}
	return false
}
