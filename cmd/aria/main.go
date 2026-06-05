// Aria backend.
//
// Composition root. The ONLY file that imports from every layer; everything
// else respects the dependency rule (outer → inner only).
//
// Provider swaps happen here. To switch from Anthropic to OpenAI, change
// the line that constructs the LLM. The application code doesn't need to
// know.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/kalman77/aria/internal/api/wsserver"
	"github.com/kalman77/aria/internal/application"
	"github.com/kalman77/aria/internal/infrastructure/anthropic"
	"github.com/kalman77/aria/internal/infrastructure/config"
	"github.com/kalman77/aria/internal/infrastructure/dicta"
	"github.com/kalman77/aria/internal/infrastructure/elevenlabs"
)

func main() {
	// Load .env from the working directory if present. Best-effort: a missing
	// file is fine, and real OS env vars set by the deployment platform always
	// win since godotenv never overrides already-set variables.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}

	logger := newSlogAdapter()

	// Read the persona prompt once at startup.
	personaPrompt, err := os.ReadFile(cfg.SystemPromptPath)
	if err != nil {
		logger.Error("read persona", "err", err)
		os.Exit(1)
	}

	// ─── Provider wiring — change these lines to swap providers ───
	//
	// Each provider implements an application port. The application code
	// only sees the interface; it has no compile-time dependency on
	// Anthropic or ElevenLabs. To add a new provider:
	//   1. Create internal/infrastructure/<vendor>/...
	//   2. Implement the relevant port (LLM / STT / TTS).
	//   3. Replace the line below.
	//   4. (No other code changes anywhere.)

	var llm application.LLM = anthropic.New(anthropic.Config{
		APIKey: cfg.AnthropicAPIKey,
		Model:  cfg.AnthropicModel,
	})

	var stt application.STT = elevenlabs.NewSTT(elevenlabs.STTConfig{
		APIKey:       cfg.ElevenLabsAPIKey,
		LanguageCode: "he",
		Logger:       logger,
	})

	var tts application.TTS = elevenlabs.NewTTS(elevenlabs.TTSConfig{
		APIKey:   cfg.ElevenLabsAPIKey,
		VoiceID:  cfg.ElevenLabsVoiceID,
		VoiceIDs: cfg.ElevenLabsVoiceIDs,
		Model:    cfg.ElevenLabsTTSModel,
		Logger:   logger,
	})

	// Hebrew niqqud preprocessor. Runs between LLM output and TTS so the
	// engine receives vocalized Hebrew text and pronounces it correctly.
	// Non-Hebrew sentences pass through unchanged.
	//
	// Set DISABLE_NIQQUD=true to skip it entirely (nil = passthrough). Useful
	// for A/B testing: some ElevenLabs Hebrew voices are tuned on plain text
	// and sound more natural without injected niqqud.
	var preprocessor application.TextPreprocessor
	if os.Getenv("DISABLE_NIQQUD") == "true" {
		logger.Info("niqqud preprocessor disabled")
	} else {
		preprocessor = dicta.New(dicta.Config{
			Logger: logger,
		})
	}

	// ─── End provider wiring ───

	srv := wsserver.New(wsserver.Config{
		STT:               stt,
		LLM:               llm,
		TTS:               tts,
		Preprocessor:      preprocessor,
		Logger:            logger,
		SystemPrompt:      string(personaPrompt),
		MaxSessionSeconds: cfg.MaxSessionSeconds,
		AllowedOrigin:     cfg.AllowedOrigin,
	})

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = httpServer.Shutdown(shutCtx)
}

// ─── Logger adapter ───

// slogAdapter implements application.Logger using the standard slog
// package. Lives here in main.go (the composition root) since it's a
// trivial adapter and doesn't deserve its own subpackage.
type slogAdapter struct {
	l *slog.Logger
}

func newSlogAdapter() *slogAdapter {
	return &slogAdapter{
		l: slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
}

func (a *slogAdapter) Info(msg string, kv ...any)  { a.l.Info(msg, kv...) }
func (a *slogAdapter) Warn(msg string, kv ...any)  { a.l.Warn(msg, kv...) }
func (a *slogAdapter) Error(msg string, kv ...any) { a.l.Error(msg, kv...) }
