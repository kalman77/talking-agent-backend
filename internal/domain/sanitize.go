package domain

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// MaxUserTextBytes caps a single user utterance.
const MaxUserTextBytes = 4096

// SanitizeUserText defends against Unicode-based prompt injection and
// oversize inputs. Pure function — same input always produces same output,
// no I/O, no globals. Lives in domain because it's a domain rule about
// what a "valid utterance" is.
//
// Strips:
//   - Tag characters (U+E0000..U+E007F) — the prompt-injection vector
//   - Bidi override / isolate controls
//   - Zero-width and BOM-style invisible characters
//   - Variation selectors and private-use-area codepoints
//
// NFC-normalizes first to collapse decomposed homoglyphs.
//
// Note: golang.org/x/text is technically a dependency. It's a Google-
// maintained extension of the standard library, distributed by the Go
// team itself. For Clean Architecture purists this is acceptable in the
// domain layer — same as importing "math" — because it's part of Go's
// extended standard library, not a third-party concern.
func SanitizeUserText(s string) string {
	if len(s) > MaxUserTextBytes {
		s = s[:MaxUserTextBytes]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}

	s = norm.NFC.String(s)

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isDangerous(r) {
			continue
		}
		if r == '\t' || r == '\n' || r == '\r' {
			b.WriteRune(r)
			continue
		}
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func isDangerous(r rune) bool {
	switch {
	case r >= 0xE0000 && r <= 0xE007F:
		return true
	case r >= 0xE0100 && r <= 0xE01EF:
		return true
	case r >= 0x202A && r <= 0x202E:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x200B || r == 0x200C || r == 0x200D ||
		r == 0xFEFF || r == 0x2060:
		return true
	case r >= 0xFE00 && r <= 0xFE0F:
		return true
	case r >= 0xE000 && r <= 0xF8FF:
		return true
	case r >= 0xF0000 && r <= 0xFFFFD:
		return true
	case r >= 0x100000 && r <= 0x10FFFD:
		return true
	}
	return false
}
