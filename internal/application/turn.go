package application

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"unicode"

	"github.com/kalman77/aria/internal/domain"
)

// turnPipeline runs one user-utterance → assistant-response cycle.
//
// Owned by ConversationService; not exposed outside the package because
// it's stateless besides what gets passed in.
type turnPipeline struct {
	llm    LLM
	tts    TTS
	sink   AudioSink
	logger Logger

	// preprocessor is optional; nil means no transformation.
	preprocessor TextPreprocessor

	systemPrompt string
}

// run executes a turn. Returns the full assistant reply on success (so
// caller can append to history) or an error. Context cancellation is the
// barge-in path; we treat it as a clean exit, not a failure.
func (p *turnPipeline) run(
	ctx context.Context,
	history domain.ChatHistory,
) (string, error) {
	// We pipe LLM output into a sentence channel. The TTS consumer reads
	// sentences in order. This lets TTS start synthesizing before the LLM
	// has finished generating, which is what makes first-audio latency
	// acceptable.
	sentences := make(chan string, 8)
	var fullReply strings.Builder
	var sentBuf bytes.Buffer

	llmDone := make(chan error, 1)
	go func() {
		err := p.llm.Stream(ctx, p.systemPrompt, history, func(delta string) {
			fullReply.WriteString(delta)
			// Emit incremental text to the UI as we go — user sees
			// progress before audio arrives.
			_ = p.sink.SendAgentEvent(ctx, "agent_partial",
				map[string]any{"text": fullReply.String()})
			flushSentences(&sentBuf, delta, sentences)
		})

		// Flush any remainder past the last sentence terminator.
		if leftover := strings.TrimSpace(sentBuf.String()); leftover != "" {
			select {
			case sentences <- leftover:
			case <-ctx.Done():
			}
		}
		close(sentences)
		llmDone <- err
	}()

	// Synthesize each sentence in order. We process serially so the
	// browser receives audio in conversation order. Parallelizing would
	// require reordering at the receiver; not worth it.
	onAudio := func(chunk AudioChunk) {
		if sendErr := p.sink.SendAudioChunk(ctx, chunk); sendErr != nil {
			p.logger.Warn("send audio chunk failed", "err", sendErr)
		}
	}

synthLoop:
	for sentence := range sentences {
		if ctx.Err() != nil {
			break
		}
		ttsText := sentence
		if p.preprocessor != nil {
			processed, err := p.preprocessor.Process(ctx, sentence)
			if err != nil {
				// Non-fatal: log and synthesize the original sentence so
				// the user still hears a response, just with imperfect
				// pronunciation.
				p.logger.Warn("text preprocess failed", "err", err)
			} else {
				ttsText = processed
			}
		}
		ttsText = normalizeNameForTTS(ttsText)

		// Split into single-script segments so a mixed-language sentence
		// (e.g. "שלום, my name is אריה") routes each segment to its own
		// voice. Single-script sentences come back as one segment, so the
		// common case pays no extra cost.
		for _, segment := range splitByScript(ttsText) {
			if ctx.Err() != nil {
				break synthLoop
			}
			if err := p.tts.Synthesize(ctx, segment, onAudio); err != nil {
				if errors.Is(err, context.Canceled) {
					break synthLoop
				}
				p.logger.Warn("tts synthesize failed", "err", err)
				break synthLoop
			}
		}
	}

	llmErr := <-llmDone
	if llmErr != nil && !errors.Is(llmErr, context.Canceled) {
		return "", llmErr
	}

	return strings.TrimSpace(fullReply.String()), ctx.Err()
}

// splitByScript breaks text into contiguous runs of the same Unicode
// script (Hebrew, Latin, Arabic, …). Non-letter runes (whitespace,
// punctuation, niqqud combining marks) attach to the current run so they
// stay glued to the word they follow.
//
// Used by the turn pipeline to route each segment of a mixed-script
// sentence to the voice configured for that language. Returns the text
// unchanged in a single segment if only one script is present, so single-
// language sentences pay no extra WS handshakes.
func splitByScript(text string) []string {
	if text == "" {
		return nil
	}

	type run struct {
		script string
		b      strings.Builder
	}
	var runs []run
	cur := -1
	for _, r := range text {
		// Non-letters cling to whatever segment is currently open. If
		// nothing is open yet (text starts with whitespace/punct), start
		// an "unknown-script" segment that the first letter will claim.
		if !unicode.IsLetter(r) {
			if cur < 0 {
				runs = append(runs, run{})
				cur = 0
			}
			runs[cur].b.WriteRune(r)
			continue
		}
		script := scriptOf(r)
		switch {
		case cur < 0:
			runs = append(runs, run{script: script})
			cur = 0
		case runs[cur].script == "":
			runs[cur].script = script
		case runs[cur].script != script:
			runs = append(runs, run{script: script})
			cur++
		}
		runs[cur].b.WriteRune(r)
	}

	out := make([]string, 0, len(runs))
	for _, r := range runs {
		s := r.b.String()
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// scriptOf returns a short tag for the Unicode script a letter belongs
// to. Latin and unrecognized scripts collapse to "latin" since they all
// route to the same default voice.
func scriptOf(r rune) string {
	switch {
	case unicode.Is(unicode.Hebrew, r):
		return "hebrew"
	case unicode.Is(unicode.Arabic, r):
		return "arabic"
	case unicode.Is(unicode.Cyrillic, r):
		return "cyrillic"
	case unicode.Is(unicode.Han, r):
		return "han"
	case unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r):
		return "kana"
	case unicode.Is(unicode.Hangul, r):
		return "hangul"
	case unicode.Is(unicode.Greek, r):
		return "greek"
	default:
		return "latin"
	}
}

// flushSentences pushes complete sentences from `delta` into ch. Anything
// past the last terminator stays buffered for the next call.
//
// Lives in this file (not domain) because it's an implementation detail
// of how we orchestrate streaming, not a domain rule.
func flushSentences(buf *bytes.Buffer, delta string, ch chan<- string) {
	buf.WriteString(delta)
	txt := buf.String()
	start := 0
	for i, r := range txt {
		if r == '.' || r == '?' || r == '!' || r == '\n' {
			end := i + 1
			sentence := strings.TrimSpace(txt[start:end])
			if sentence != "" {
				ch <- sentence
			}
			start = end
		}
	}
	buf.Reset()
	if start < len(txt) {
		buf.WriteString(txt[start:])
	}
}

// normalizeNameForTTS rewrites the various dotted/spaced spellings of the
// assistant's name into "Aria" so the TTS reads it as one word instead of
// pronouncing each letter. Belt-and-suspenders alongside the persona rule.
var nameVariants = []string{
	"A.R.I.A.",
	"A.R.I.A",
	"A. R. I. A.",
	"A R I A",
	"a.r.i.a.",
	"a.r.i.a",
}

func normalizeNameForTTS(s string) string {
	for _, v := range nameVariants {
		s = strings.ReplaceAll(s, v, "Aria")
	}
	return s
}
