package application

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/kalman77/aria/internal/domain"
)

// ConversationService is the central use case. One instance per active
// conversation. Owns:
//   - The conversation history
//   - The current turn lifecycle (cancellation, generation counter)
//   - Coordination between STT events and TTS output
//
// The API layer instantiates one of these per WebSocket connection, calls
// Run to drive the pipeline, and disposes when the connection closes.
//
// This struct is what the rest of the world calls "the application." It
// has no knowledge of HTTP, WebSockets, JSON, or any specific provider.
type ConversationService struct {
	stt          STT
	llm          LLM
	tts          TTS
	preprocessor TextPreprocessor
	sink         AudioSink
	logger       Logger
	systemPrompt string

	// Mutable state guarded by mu.
	mu      sync.Mutex
	history domain.ChatHistory

	// Turn lifecycle. Each new turn cancels the previous; turnGen is
	// atomic so stale TTS goroutines can detect they've been superseded
	// and bail.
	turnCancel context.CancelFunc
	turnGen    atomic.Int64
}

// ConversationConfig bundles the dependencies for clarity at the call site.
type ConversationConfig struct {
	STT          STT
	LLM          LLM
	TTS          TTS
	Preprocessor TextPreprocessor // optional
	Sink         AudioSink
	Logger       Logger
	SystemPrompt string
}

// NewConversationService constructs a service. Doesn't start anything;
// call Run to begin processing.
func NewConversationService(cfg ConversationConfig) *ConversationService {
	return &ConversationService{
		stt:          cfg.STT,
		llm:          cfg.LLM,
		tts:          cfg.TTS,
		preprocessor: cfg.Preprocessor,
		sink:         cfg.Sink,
		logger:       cfg.Logger,
		systemPrompt: cfg.SystemPrompt,
		history:      domain.ChatHistory{},
	}
}

// Run drives the conversation until ctx is cancelled or audioFrames
// closes. Audio frames flow in from the API layer; user-typed text comes
// in on userTexts; transcript events are handled internally; TTS output
// flows out via the AudioSink.
//
// This is the main use case. From the API layer's perspective, it's the
// single function that "runs the conversation."
func (s *ConversationService) Run(
	ctx context.Context,
	audioFrames <-chan []byte,
	userTexts <-chan string,
) {
	transcripts := make(chan domain.TranscriptEvent, 32)

	// STT runs in its own goroutine; emits events on `transcripts`. It
	// will close the channel when it exits, which lets us range cleanly.
	go func() {
		if err := s.stt.Transcribe(ctx, audioFrames, transcripts); err != nil {
			s.logger.Warn("stt transcribe ended with error", "err", err)
		}
	}()

	// Main loop: react to transcript events OR typed user messages.
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-transcripts:
			if !ok {
				return
			}
			s.handleTranscript(ctx, ev)
		case text, ok := <-userTexts:
			if !ok {
				userTexts = nil // disable case
				continue
			}
			s.handleTypedText(ctx, text)
		}
	}
}

// handleTypedText treats a typed message exactly like a final user
// utterance: echo it back to the client (so it lands in the transcript)
// and kick off a turn.
func (s *ConversationService) handleTypedText(ctx context.Context, text string) {
	// Barge-in if we were mid-utterance — user just sent a new message.
	s.mu.Lock()
	bargeNeeded := s.turnCancel != nil
	s.mu.Unlock()
	if bargeNeeded {
		s.bargeIn()
	}
	_ = s.sink.SendAgentEvent(ctx, "user_final", map[string]any{"text": text})
	s.startTurn(ctx, text)
}

func (s *ConversationService) handleTranscript(
	ctx context.Context,
	ev domain.TranscriptEvent,
) {
	switch ev.Kind {
	case domain.TranscriptPartial:
		// User is still speaking. If we're mid-utterance, this is barge-in.
		s.mu.Lock()
		bargeNeeded := s.turnCancel != nil
		s.mu.Unlock()
		if bargeNeeded {
			s.bargeIn()
		}
		_ = s.sink.SendAgentEvent(ctx, "user_partial", map[string]any{"text": ev.Text})

	case domain.TranscriptFinal:
		_ = s.sink.SendAgentEvent(ctx, "user_final", map[string]any{"text": ev.Text})
		s.startTurn(ctx, ev.Text)
	}
}

func (s *ConversationService) bargeIn() {
	s.mu.Lock()
	cancel := s.turnCancel
	s.turnCancel = nil
	s.mu.Unlock()

	if cancel != nil {
		s.logger.Info("barge-in")
		cancel()
		_ = s.sink.SendAgentEvent(context.Background(), "agent_interrupted", nil)
	}
}

// startTurn launches a new turn for the given user utterance. Sanitizes
// input first (domain rule), then runs the pipeline asynchronously so the
// main loop stays responsive to barge-in and the next utterance.
func (s *ConversationService) startTurn(parentCtx context.Context, userText string) {
	userText = domain.SanitizeUserText(userText)
	if userText == "" {
		return
	}

	// Cancel any in-flight turn and record the new one.
	s.mu.Lock()
	if s.turnCancel != nil {
		s.turnCancel()
	}
	turnCtx, cancel := context.WithCancel(parentCtx)
	s.turnCancel = cancel
	s.history = s.history.Append(domain.ChatTurn{
		Role:    domain.RoleUser,
		Content: userText,
	})
	historySnapshot := s.history
	s.mu.Unlock()

	gen := s.turnGen.Add(1)

	go func() {
		defer cancel()

		pipeline := &turnPipeline{
			llm:          s.llm,
			tts:          s.tts,
			preprocessor: s.preprocessor,
			sink:         s.sink,
			logger:       s.logger,
			systemPrompt: s.systemPrompt,
		}

		reply, err := pipeline.run(turnCtx, historySnapshot)
		if err != nil && err != context.Canceled {
			s.logger.Warn("turn pipeline error", "err", err)
		}

		// If a newer turn started while we were running, our reply is
		// stale — don't append it to history and don't broadcast finality.
		if gen != s.turnGen.Load() {
			return
		}
		if reply != "" {
			s.mu.Lock()
			s.history = s.history.Append(domain.ChatTurn{
				Role:    domain.RoleAssistant,
				Content: reply,
			})
			s.mu.Unlock()
			_ = s.sink.SendAgentEvent(turnCtx, "agent_final", map[string]any{"text": reply})
		}
	}()
}
