// Package domain holds the core types and rules of Aria — the conversation
// itself. No external dependencies, not even "context" beyond what types
// genuinely need.
//
// The classic Clean Architecture rule: if you removed every line of
// infrastructure code (HTTP, ElevenLabs, Anthropic), this package would
// still compile. It describes "what a conversation is," not "how we move
// audio bytes around."
package domain

// Role is who said what.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// ChatTurn is one conversational turn — a user utterance or an assistant
// reply. Immutable once constructed; new history is built by appending.
type ChatTurn struct {
	Role    Role
	Content string
}

// ChatHistory is an ordered list of turns. We use a typed slice instead of
// []ChatTurn directly so the domain owns the operations on it.
type ChatHistory []ChatTurn

// Append returns a new history with the turn added. Doesn't mutate the
// receiver — important because the application layer may pass histories
// across goroutines.
func (h ChatHistory) Append(turn ChatTurn) ChatHistory {
	out := make(ChatHistory, len(h)+1)
	copy(out, h)
	out[len(h)] = turn
	return out
}

// Last returns the most recent turn, or zero value + false if empty.
func (h ChatHistory) Last() (ChatTurn, bool) {
	if len(h) == 0 {
		return ChatTurn{}, false
	}
	return h[len(h)-1], true
}
