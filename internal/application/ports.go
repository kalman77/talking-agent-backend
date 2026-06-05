// Package application contains the use cases and the port interfaces they
// require. The interfaces are deliberately defined HERE, not in
// infrastructure — that's the dependency inversion that makes Clean
// Architecture's "swap any provider" work.
//
// Concretely: when you write an OpenAI implementation, OpenAI imports
// THIS package (to know the LLM interface), not the other way around.
// The application has no idea OpenAI exists.
package application

import (
	"context"

	"github.com/kalman77/aria/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────────
// STT — speech-to-text streaming.
// ─────────────────────────────────────────────────────────────────────────

// STT transcribes streamed audio into TranscriptEvents.
//
// Contract:
//   - Implementations open a streaming session on call, run until ctx is
//     done or audioFrames closes, and emit events on `events`.
//   - Implementations MUST close `events` on exit so callers can range
//     over it without leaking goroutines.
//   - audioFrames carries PCM16 mono 16kHz frames. Implementations should
//     resample if their backend wants something else; the application
//     layer doesn't care.
//   - Partial events fire as the user speaks. Final events fire at
//     end-of-utterance (server-side VAD or equivalent).
//   - If the implementation hits a fatal error mid-stream, it returns it
//     and closes `events`. Recoverable errors are swallowed and logged.
type STT interface {
	Transcribe(
		ctx context.Context,
		audioFrames <-chan []byte,
		events chan<- domain.TranscriptEvent,
	) error
}

// ─────────────────────────────────────────────────────────────────────────
// LLM — text generation with streaming output.
// ─────────────────────────────────────────────────────────────────────────

// LLM generates an assistant response given a system prompt and history.
//
// Contract:
//   - `system` is the persona prompt. Implementations must pass it as a
//     privileged instruction layer, not concatenated to the user messages.
//     For Anthropic this is the `system` field; for OpenAI a system role
//     message; for local models depends on chat template.
//   - `history` is alternating user/assistant turns ending with a user
//     turn. Implementations should NOT add the last user message to the
//     turn (it's already in history).
//   - onDelta is invoked for each text fragment as it arrives. May be
//     called many times; callers buffer/aggregate as needed.
//   - Returns when generation completes naturally or ctx is cancelled.
//     Cancellation is the barge-in mechanism; implementations must close
//     their upstream connection promptly so token billing stops.
type LLM interface {
	Stream(
		ctx context.Context,
		system string,
		history domain.ChatHistory,
		onDelta func(text string),
	) error
}

// ─────────────────────────────────────────────────────────────────────────
// TTS — text-to-speech synthesis with streamed audio output.
// ─────────────────────────────────────────────────────────────────────────

// AudioChunk is one piece of synthesized audio.
type AudioChunk struct {
	PCM        []byte // PCM16 mono samples
	SampleRate int    // Hz, e.g. 16000 or 24000
}

// TTS synthesizes speech for one utterance.
//
// Contract:
//   - One call = one utterance. Don't try to stitch multiple sentences
//     into a single call; the caller (turn pipeline) handles ordering.
//   - onAudio is invoked with PCM chunks as they arrive. The first chunk
//     should arrive as quickly as the provider allows — first-audio
//     latency is the dominant UX metric.
//   - SampleRate may differ between providers but should be consistent
//     across calls to the same instance. Reported on each chunk so the
//     consumer doesn't have to track it separately.
//   - Returns when synthesis completes or ctx is cancelled. Cancellation
//     must abort cleanly — barge-in stops the audio mid-utterance.
type TTS interface {
	Synthesize(
		ctx context.Context,
		text string,
		onAudio func(chunk AudioChunk),
	) error
}

// ─────────────────────────────────────────────────────────────────────────
// TextPreprocessor — optional transformation applied to a sentence before
// it is handed to TTS. Used, e.g., to add Hebrew niqqud (vowel marks) so
// the TTS engine pronounces ambiguous Hebrew words correctly.
// ─────────────────────────────────────────────────────────────────────────

// TextPreprocessor transforms a sentence prior to synthesis.
//
// Contract:
//   - Pure transformation: input text in, output text out. No side effects.
//   - Must be safe to skip — implementations should return the input
//     unchanged when the transformation does not apply (e.g., non-Hebrew
//     text for a niqqud processor).
//   - Errors are non-fatal to the turn: callers should log and fall back
//     to the original text.
type TextPreprocessor interface {
	Process(ctx context.Context, text string) (string, error)
}

// ─────────────────────────────────────────────────────────────────────────
// AudioSink — where TTS audio goes.
// ─────────────────────────────────────────────────────────────────────────

// AudioSink is the consumer of synthesized audio. Typically the WebSocket
// to the browser. Defining it as a port (rather than just having the API
// layer hold a WS reference) means the application logic stays
// transport-agnostic — you could swap the browser sink for a file-writing
// sink in tests with no code changes.
type AudioSink interface {
	// SendAudioChunk delivers one PCM chunk. Returns error on transport
	// failure; the caller decides whether to abort the turn.
	SendAudioChunk(ctx context.Context, chunk AudioChunk) error

	// SendAgentEvent reports a high-level state change to the consumer
	// (e.g., "agent_interrupted", "agent_partial:text"). Stringly-typed on
	// purpose — the application emits semantic events, the API layer
	// translates them to whatever wire format it uses.
	SendAgentEvent(ctx context.Context, kind string, payload map[string]any) error
}

// ─────────────────────────────────────────────────────────────────────────
// Logger — structured logging port.
// ─────────────────────────────────────────────────────────────────────────

// Logger is the application's view of logging. Defined as a port so the
// application doesn't import any specific logger (slog, zap, logrus).
// Infrastructure provides an adapter; tests can pass a no-op or capture.
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
}
