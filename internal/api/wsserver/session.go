package wsserver

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"github.com/kalman77/aria/internal/application"
)

// handleSession upgrades to WebSocket and runs one ConversationService for
// the lifetime of the connection.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.originPatterns,
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	sessionCtx, cancel := s.makeSessionContext(r.Context())
	defer cancel()

	// Wait for hello — the browser tells us it's ready.
	_, hello, err := conn.Read(sessionCtx)
	if err != nil {
		return
	}
	var helloMsg struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(hello, &helloMsg); err != nil || helloMsg.Type != "hello" {
		return
	}

	// The sink is THIS package's job — it knows the wire format.
	sink := newWSSink(conn)

	// Build the use case with all its dependencies.
	conv := application.NewConversationService(application.ConversationConfig{
		STT:          s.stt,
		LLM:          s.llm,
		TTS:          s.tts,
		Preprocessor: s.preprocessor,
		Sink:         sink,
		Logger:       s.logger,
		SystemPrompt: s.systemPrompt,
	})

	// Mic audio flows in on this channel; we read WS frames and forward.
	audioFrames := make(chan []byte, 32)
	// Typed user messages — alternative to voice input.
	userTexts := make(chan string, 4)

	// Frame reader goroutine.
	go func() {
		defer close(audioFrames)
		defer close(userTexts)
		for {
			kind, data, err := conn.Read(sessionCtx)
			if err != nil {
				return
			}
			switch kind {
			case websocket.MessageBinary:
				select {
				case audioFrames <- data:
				default:
					// Backpressure: drop rather than block.
					s.logger.Warn("audio chan full; dropping frame")
				}
			case websocket.MessageText:
				var ctrl struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(data, &ctrl); err != nil {
					continue
				}
				if ctrl.Type == "user_text" && ctrl.Text != "" {
					select {
					case userTexts <- ctrl.Text:
					default:
						s.logger.Warn("user_text chan full; dropping message")
					}
				}
			}
		}
	}()

	// Run the use case until ctx done or audio channel closes.
	conv.Run(sessionCtx, audioFrames, userTexts)

	conn.Close(websocket.StatusNormalClosure, "bye")
}

// ─── AudioSink implementation ───

// wsSink is the API-layer implementation of application.AudioSink. It
// knows how to translate semantic events (agent_partial, audio chunks)
// into the JSON+binary wire protocol the browser expects.
//
// This is the dependency-inversion glue: application defines what it
// needs (an AudioSink), this package implements it. The application has
// no idea WebSockets exist.
var _ application.AudioSink = (*wsSink)(nil)

type wsSink struct {
	conn    *websocket.Conn
	writeMu sync.Mutex // serializes writes — text headers must precede their binary frames atomically
}

func newWSSink(conn *websocket.Conn) *wsSink {
	return &wsSink{conn: conn}
}

func (s *wsSink) SendAudioChunk(ctx context.Context, chunk application.AudioChunk) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	header, err := json.Marshal(map[string]any{
		"type":        "tts_chunk_header",
		"sample_rate": chunk.SampleRate,
		"bytes":       len(chunk.PCM),
	})
	if err != nil {
		return err
	}
	if err := s.conn.Write(ctx, websocket.MessageText, header); err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageBinary, chunk.PCM)
}

func (s *wsSink) SendAgentEvent(ctx context.Context, kind string, payload map[string]any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	out := map[string]any{"type": kind}
	for k, v := range payload {
		out[k] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageText, b)
}
