# A.R.I.A. Backend

Real-time voice agent backend for the A.R.I.A. portfolio assistant. Accepts a
WebSocket connection from the browser, streams microphone audio through
speech-to-text, generates a reply with an LLM, and streams synthesized speech
back — with barge-in (interrupt) support and bilingual Hebrew/English handling.

Written in Go, structured with Clean Architecture so any provider (LLM, STT,
TTS) can be swapped by changing one line in the composition root.

---

## Table of contents

- [Architecture](#architecture)
- [Directory layout](#directory-layout)
- [Data flow](#data-flow)
- [WebSocket wire protocol](#websocket-wire-protocol)
- [Providers](#providers)
- [Configuration](#configuration)
- [Running locally](#running-locally)
- [HTTPS & microphone access](#https--microphone-access)
- [Production deployment](#production-deployment)
- [Swapping providers](#swapping-providers)
- [Voice routing & languages](#voice-routing--languages)
- [Troubleshooting](#troubleshooting)

---

## Architecture

Clean Architecture with strict dependency inversion. Dependencies point
**inward**: infrastructure depends on application, application depends on
domain, domain depends on nothing.

```
        ┌─────────────────────────────────────────────┐
        │  cmd/aria  (composition root / main)         │  wires everything
        └───────────────┬─────────────────────────────┘
                        │ constructs
   ┌────────────────────┼─────────────────────────────┐
   │                    │                              │
┌──▼───────────┐  ┌─────▼──────────┐         ┌─────────▼──────────┐
│ api/wsserver │  │ infrastructure │         │   application      │
│  (transport) │  │ (providers)    │ ──────▶ │  (use cases +      │
│              │  │ anthropic,     │ implements│  port interfaces) │
│  implements  │  │ elevenlabs,    │  ports  │                    │
│  AudioSink   │  │ dicta, config  │         │  depends on ▼      │
└──────┬───────┘  └────────────────┘         └─────────┬──────────┘
       │                                               │
       └──────────────────────────────────────────────┤
                                              ┌─────────▼──────────┐
                                              │      domain        │
                                              │ (entities, rules)  │
                                              └────────────────────┘
```

The **port interfaces** (`LLM`, `STT`, `TTS`, `TextPreprocessor`, `AudioSink`,
`Logger`) are defined in `internal/application/ports.go`. Providers import the
application package to implement them — the application has no compile-time
knowledge of Anthropic, ElevenLabs, or WebSockets.

---

## Directory layout

```
backend/
├── cmd/aria/
│   └── main.go                  # Composition root: loads config, wires providers, runs HTTP server
├── internal/
│   ├── api/wsserver/            # Transport layer (WebSocket)
│   │   ├── server.go            #   HTTP routes, CORS, origin handling
│   │   └── session.go           #   Per-connection upgrade, frame reader, AudioSink impl
│   ├── application/             # Use cases + port interfaces (the core)
│   │   ├── ports.go             #   LLM / STT / TTS / TextPreprocessor / AudioSink / Logger
│   │   ├── conversation.go      #   ConversationService: history, turn lifecycle, barge-in
│   │   └── turn.go              #   turnPipeline: LLM→sentence split→preprocess→TTS, script routing
│   ├── domain/                  # Pure entities & rules, no dependencies
│   │   ├── chat.go              #   ChatHistory, ChatTurn, roles
│   │   ├── transcript.go        #   TranscriptEvent (partial/final)
│   │   └── sanitize.go          #   User-text sanitation
│   └── infrastructure/          # Provider adapters
│       ├── anthropic/claude.go  #   LLM (streaming)
│       ├── elevenlabs/stt.go    #   STT (Scribe v2 Realtime, WebSocket)
│       ├── elevenlabs/tts.go    #   TTS (WebSocket stream-input)
│       ├── dicta/nakdan.go      #   Hebrew niqqud preprocessor
│       └── config/config.go     #   Env-var configuration
└── go.mod
```

---

## Data flow

One conversation turn, voice path:

```
Browser mic (PCM16 mono 16kHz)
   │  binary WS frames
   ▼
wsserver.handleSession ──▶ audioFrames chan ──▶ ConversationService.Run
   │
   ▼
STT (ElevenLabs Scribe v2 Realtime) ──▶ TranscriptEvent {partial|final}
   │                                         │
   │ partial → "user_partial" event          │ (partial while mid-reply = barge-in → cancel turn)
   │                                          ▼
   │                                    final → startTurn(text)
   │                                          ▼
   │                                    turnPipeline.run:
   │                                      LLM.Stream (Anthropic) ──▶ deltas
   │                                          │ flushSentences (split on . ? ! \n)
   │                                          ▼
   │                                      per sentence:
   │                                        TextPreprocessor (Dicta niqqud, Hebrew only)
   │                                        normalizeNameForTTS ("A.R.I.A." → "Aria")
   │                                        splitByScript (route mixed He/En segments)
   │                                          ▼
   │                                        TTS.Synthesize (ElevenLabs) ──▶ AudioChunk (PCM16)
   ▼                                          ▼
"user_final" / "agent_partial" / "agent_final" + tts_chunk_header + binary PCM ──▶ Browser
```

Typed text (no mic) enters via a `user_text` control message and is treated
exactly like a final transcript.

**Barge-in:** a new partial transcript or typed message while the agent is
speaking cancels the in-flight turn's context, which aborts LLM generation and
TTS mid-stream and emits `agent_interrupted`.

---

## WebSocket wire protocol

Endpoint: `GET /session` (upgrades to WebSocket). Health: `GET /healthz` → `ok`.

### Client → server

| Message | Form | Meaning |
|---|---|---|
| hello | text JSON `{"type":"hello"}` | Required first message; readiness handshake. |
| audio | **binary** | PCM16 mono 16 kHz frames from the mic. |
| typed text | text JSON `{"type":"user_text","text":"..."}` | Alternative to voice input. |

### Server → client

| Message | Form | Meaning |
|---|---|---|
| `user_partial` | text JSON `{type,text}` | Live (interim) transcript of the user. |
| `user_final` | text JSON `{type,text}` | Finalized user utterance. |
| `agent_partial` | text JSON `{type,text}` | Incremental assistant text as the LLM streams. |
| `agent_final` | text JSON `{type,text}` | Full assistant reply (appended to history). |
| `agent_interrupted` | text JSON `{type}` | Turn cancelled by barge-in. |
| `tts_chunk_header` | text JSON `{type,sample_rate,bytes}` | Precedes each audio frame. |
| audio | **binary** | PCM16 audio chunk (sample rate per the preceding header, 24 kHz). |

Text headers and their binary frames are written under a mutex so a header
always immediately precedes its PCM payload.

---

## Providers

| Port | Provider | Notes |
|---|---|---|
| **LLM** | Anthropic Claude | Streaming. Model via `CLAUDE_MODEL` (default `claude-sonnet-4-6`). |
| **STT** | ElevenLabs Scribe v2 Realtime | WebSocket. Model `scribe_v2_realtime`, VAD commit strategy, language `he`. Sends PCM16 @ 16 kHz. |
| **TTS** | ElevenLabs | WebSocket `stream-input`. `output_format=pcm_24000`, `auto_mode=true`. Per-language voice routing. |
| **Preprocessor** | Dicta Nakdan | Adds Hebrew niqqud before TTS. Free public endpoint, no key. Optional. |

---

## Configuration

Config is loaded from environment variables (`internal/infrastructure/config`).
A `.env` file in the working directory is loaded via `godotenv` if present —
**real OS env vars always win**, so in production you inject env vars directly
and ship no `.env`.

| Variable | Required | Default | Description |
|---|---|---|---|
| `PORT` | no | `8080` | HTTP listen port. |
| `ALLOWED_ORIGIN` | no | `*` | Comma-separated allowed origins, or `*` for all. Lock down in prod. |
| `MAX_SESSION_SECONDS` | no | `300` | Hard cap on a session's duration. |
| `SYSTEM_PROMPT_PATH` | no | `prompts/persona_aria.md` | Persona prompt file, relative to run dir. |
| `ANTHROPIC_API_KEY` | **yes** | — | Anthropic key. |
| `CLAUDE_MODEL` | no | `claude-sonnet-4-6` | LLM model id. |
| `ELEVENLABS_API_KEY` | **yes** | — | ElevenLabs key (STT + TTS). |
| `ELEVENLABS_VOICE_ID` | conditional | — | Default/fallback voice. Required **unless** `ELEVENLABS_VOICE_IDS` is set. |
| `ELEVENLABS_VOICE_IDS` | conditional | — | Per-language voices: `he:ID,en:ID`. Satisfies the voice requirement on its own. |
| `ELEVENLABS_TTS_MODEL` | no | `eleven_multilingual_v2` | TTS model. See [Troubleshooting](#troubleshooting). |
| `DISABLE_NIQQUD` | no | — | Set `true` to skip the Hebrew niqqud preprocessor. |

Startup fails fast if `ANTHROPIC_API_KEY`, `ELEVENLABS_API_KEY`, or a voice
source is missing.

Copy `.env.example` to `.env` and fill it in.

---

## Running locally

**Prerequisites:** Go 1.25+, an Anthropic key, an ElevenLabs key.

```bash
cp .env.example .env   # then fill in the keys
go run ./cmd/aria
```

You should see `{"level":"INFO","msg":"listening","addr":":8080"}`.

> Run from the project root (where `go.mod`, `.env`, and `prompts/` live). The
> default paths (`.env`, `prompts/persona_aria.md`) resolve relative to the
> working directory. Running elsewhere requires absolute paths or directly
> exported env vars.

### Windows note (Smart App Control)

If `go run` fails with *"An Application Control policy has blocked this file"*,
Windows Smart App Control is blocking the unsigned binary from the build cache.
Smart App Control can only be turned **off permanently** (not re-enabled without
reinstalling Windows). Easiest workaround: run under **WSL2** (Linux binaries
are not governed by it):

```bash
wsl
go run ./cmd/aria   # use forward slashes in WSL
```

---

## HTTPS & microphone access

Browsers expose `navigator.mediaDevices.getUserMedia` (the mic) **only in a
secure context**: HTTPS, or `http://localhost`. On a phone hitting a dev server
by LAN IP over plain HTTP, `mediaDevices` is `undefined` and the mic fails.

Because the frontend rewrites the backend URL scheme (`http→ws`, `https→wss`),
an HTTPS frontend forces a **`wss://` backend**: serving the page over HTTPS
while pointing at an `http://`/`ws://` backend triggers a mixed-content block.

**Rule of thumb:** in any non-localhost setting, both the frontend and this
backend must be served over TLS.

---

## Production deployment

**Secrets never live in the repo or image — the platform injects them at
runtime.** `godotenv` only reads `.env` if present and never overrides real env
vars, so production = set real env vars, ship no `.env`.

- **PaaS (Render / Railway / Fly.io / Cloud Run):** push code (no `.env`), set
  the secrets in the platform's encrypted env/secrets UI. TLS is provided
  automatically → frontend gets `wss://` for free.
- **Docker on a VPS:** keep secrets out of the image. Inject at run:
  ```bash
  docker run --env-file /etc/aria/aria.env -p 8080:8080 aria   # file chmod 600, never committed
  ```
- **systemd on a VPS:**
  ```ini
  [Service]
  EnvironmentFile=/etc/aria/aria.env   # root-only, chmod 600, outside the repo
  ExecStart=/opt/aria/aria
  ```

Production checklist:
1. `.gitignore` the `.env` before any `git init` (already configured at the repo root).
2. Set `ALLOWED_ORIGIN` to your real origin(s), not `*`.
3. Put the service behind TLS (reverse proxy or platform).
4. Rotate any key that has ever been committed or shared.

---

## Swapping providers

Implement the relevant port (`LLM` / `STT` / `TTS`) in a new
`internal/infrastructure/<vendor>/` package, then change one line in the
provider-wiring block of `cmd/aria/main.go`:

```go
var llm application.LLM = anthropic.New(anthropic.Config{ ... })
//                        ^^^ replace with your provider; no other code changes
```

The application and domain layers are untouched because they only see the
interface.

---

## Voice routing & languages

TTS voice is chosen per synthesized segment:

1. `splitByScript` (in `turn.go`) breaks a mixed-language sentence into
   single-script segments (`"שלום, my name is אריה"` → Hebrew + Latin segments).
2. For each segment, `pickVoice` (in `tts.go`) uses `detectLang` (dominant
   Unicode script) to look up `ELEVENLABS_VOICE_IDS`:
   - Hebrew → `he:` voice
   - Latin/English → `en:` voice
   - unlisted script → falls back to `ELEVENLABS_VOICE_ID` (or, if unset, an
     entry from the map, preferring `en`).

The chosen voice, detected language, model, and text are logged at INFO
(`msg:"tts voice"`) for debugging pronunciation issues.

---

## Troubleshooting

**`config load err="required env var ... not set"`**
The `.env` isn't being read or a required var is empty. Run from the project
root so `.env` resolves, or export the vars directly. A voice is required: set
either `ELEVENLABS_VOICE_ID` or `ELEVENLABS_VOICE_IDS`.

**`tts dial: ... expected handshake response status code 101 but got 403`**
ElevenLabs rejected the TTS WebSocket. Usual cause: `ELEVENLABS_TTS_MODEL` is a
model not served on the realtime `stream-input` endpoint (e.g. `eleven_v3`,
which is alpha/gated). Use a streaming-capable model such as
`eleven_turbo_v2_5` or `eleven_multilingual_v2`.

**Voice sounds accented vs. the ElevenLabs playground**
Accent comes from the voice + model + settings, not a parameter. Things that
differ from the playground:
- Model mismatch — match `ELEVENLABS_TTS_MODEL` to the playground's model.
- This backend sends no `voice_settings`, so it uses the voice's saved
  defaults; if you moved sliders (stability/style/etc.) in the playground,
  those aren't applied here.
- Per-sentence/per-segment synthesis loses cross-sentence prosody context.
- Niqqud: the Dicta preprocessor injects vowel marks; toggle with
  `DISABLE_NIQQUD=true` to compare. The exact string sent to TTS is logged
  (`msg:"tts voice"`, field `text`) so you can paste it into the playground.

**`Cannot read properties of undefined (reading 'getUserMedia')` (frontend)**
The page isn't in a secure context. Serve over HTTPS or `localhost`. See
[HTTPS & microphone access](#https--microphone-access).
