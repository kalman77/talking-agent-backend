package domain

// TranscriptKind distinguishes interim from final transcripts.
type TranscriptKind int

const (
	// TranscriptPartial is an in-progress transcription that may change as
	// the user keeps speaking. UI shows it greyed out; logic shouldn't act
	// on it as final speech.
	TranscriptPartial TranscriptKind = iota

	// TranscriptFinal is committed by the STT and won't change. This is
	// what triggers a turn.
	TranscriptFinal
)

// TranscriptEvent is one transcription emission from an STT provider.
//
// Domain-level type because the application layer reasons about partials
// (which trigger barge-in) and finals (which trigger turns) — that's a
// conversational concept, not an implementation detail of any particular
// STT vendor.
type TranscriptEvent struct {
	Kind TranscriptKind
	Text string
}
