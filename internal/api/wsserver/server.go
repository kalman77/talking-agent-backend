// Package wsserver is the WebSocket-driven API layer. It accepts a
// connection, builds a ConversationService for it, and translates between
// the wire protocol and the application's use cases.
//
// This package depends on application (to call into the use case) and
// domain (for shared types). It does NOT depend on any specific provider.
package wsserver

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/kalman77/aria/internal/application"
)

// Server is the HTTP layer. Holds the dependencies needed to construct a
// ConversationService for each new WebSocket connection.
type Server struct {
	stt           application.STT
	llm           application.LLM
	tts           application.TTS
	preprocessor  application.TextPreprocessor
	logger        application.Logger
	systemPrompt  string
	maxSessionSec int

	// allowAllOrigins is true when "*" is configured — every origin is
	// accepted and CORS echoes "*".
	allowAllOrigins bool
	// allowedOrigins holds the raw configured origins (with scheme), used to
	// echo back the matching Origin in the CORS header.
	allowedOrigins []string
	// originPatterns holds host-only patterns ("yoursite.com", "*.foo.com")
	// for the WebSocket upgrade origin check. The coder/websocket library
	// matches these against the Origin header's host, not the full URL.
	originPatterns []string
}

type Config struct {
	STT               application.STT
	LLM               application.LLM
	TTS               application.TTS
	Preprocessor      application.TextPreprocessor // optional
	Logger            application.Logger
	SystemPrompt      string
	MaxSessionSeconds int
	AllowedOrigin     string
}

func New(cfg Config) *Server {
	s := &Server{
		stt:           cfg.STT,
		llm:           cfg.LLM,
		tts:           cfg.TTS,
		preprocessor:  cfg.Preprocessor,
		logger:        cfg.Logger,
		systemPrompt:  cfg.SystemPrompt,
		maxSessionSec: cfg.MaxSessionSeconds,
	}
	// AllowedOrigin is a comma-separated list. "*" anywhere means allow all.
	for _, o := range strings.Split(cfg.AllowedOrigin, ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if o == "*" {
			s.allowAllOrigins = true
			s.originPatterns = []string{"*"}
			continue
		}
		s.allowedOrigins = append(s.allowedOrigins, o)
		s.originPatterns = append(s.originPatterns, originHost(o))
	}
	return s
}

// originHost reduces a configured origin to the host[:port] form that the
// WebSocket library matches against. A bare host (or "*.foo.com" pattern) is
// returned unchanged; a full URL has its scheme and path stripped.
func originHost(o string) string {
	if i := strings.Index(o, "://"); i != -1 {
		o = o[i+3:]
	}
	if i := strings.IndexByte(o, '/'); i != -1 {
		o = o[:i]
	}
	return o
}

// Handler returns an http.Handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/session", s.handleSession)
	return s.cors(mux)
}

func (s *Server) cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := s.corsOrigin(r.Header.Get("Origin")); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// corsOrigin returns the value to send in Access-Control-Allow-Origin: "*"
// when all origins are allowed, the request's own Origin when it is on the
// allowlist, or "" to omit the header entirely.
func (s *Server) corsOrigin(reqOrigin string) string {
	if s.allowAllOrigins {
		return "*"
	}
	for _, o := range s.allowedOrigins {
		if o == reqOrigin {
			return reqOrigin
		}
	}
	return ""
}

// makeSessionContext bounds a session by the configured max duration.
func (s *Server) makeSessionContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, time.Duration(s.maxSessionSec)*time.Second)
}
