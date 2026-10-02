// Package jev is a minimal client for TypeSafe's Jev System One model.
//
// Jev evaluates a single state against typed questions (Noul, Choice and
// Score) and answers each one with calibrated probabilities. See
// https://docs.typesafe.ai for the full API reference.
package jev

// QuestionType identifies the kind of answer Jev returns for a question.
type QuestionType string

const (
	// TypeNoul asks a yes/no question and answers with P(true).
	TypeNoul QuestionType = "noul"
	// TypeChoice asks Jev to pick one option from a named set.
	TypeChoice QuestionType = "choice"
	// TypeScore asks Jev to place the state on an ordered scale.
	TypeScore QuestionType = "score"
)

// Question is a single typed question evaluated against the request state.
//
// Criteria depends on Type: a map with "true"/"false" keys for Noul, a map of
// option name to description for Choice, and an ordered slice of level
// descriptions (worst to best) for Score. Prefer the Noul, Choice and Score
// constructors over building this struct by hand.
type Question struct {
	Type         QuestionType `json:"type"`
	Instructions string       `json:"instructions"`
	Criteria     any          `json:"criteria,omitempty"`
}

// Noul builds a yes/no question.
//
// Parameters:
//   - instructions (string): the question Jev answers.
//   - ifTrue (string): what a "true" answer means.
//   - ifFalse (string): what a "false" answer means.
func Noul(instructions, ifTrue, ifFalse string) Question {
	return Question{
		Type:         TypeNoul,
		Instructions: instructions,
		Criteria:     map[string]string{"true": ifTrue, "false": ifFalse},
	}
}

// Choice builds a categorical question.
//
// Parameters:
//   - instructions (string): the question Jev answers.
//   - options (map[string]string): option name to description; at most 255.
func Choice(instructions string, options map[string]string) Question {
	return Question{Type: TypeChoice, Instructions: instructions, Criteria: options}
}

// Score builds an ordinal question.
//
// Parameters:
//   - instructions (string): the question Jev answers.
//   - levels (...string): 2 to 10 level descriptions, ordered worst to best.
func Score(instructions string, levels ...string) Question {
	return Question{Type: TypeScore, Instructions: instructions, Criteria: levels}
}

// Request is the body sent to the System One endpoint.
type Request struct {
	// Model is a model alias such as "jev-latest" or a pinned version. When
	// empty, the client fills in its configured default.
	Model string `json:"model"`
	// State is the context Jev evaluates: a string, a JSON-encodable value,
	// or a slice of strings.
	State any `json:"state"`
	// Questions maps a caller-chosen key to the question to evaluate. All
	// questions are answered in a single round trip.
	Questions map[string]Question `json:"questions"`
}

// Answer is Jev's typed answer to a single question. Which fields are set
// depends on Type.
type Answer struct {
	Type QuestionType `json:"type"`
	// Noul is P(true) for Noul questions.
	Noul float64 `json:"noul,omitempty"`
	// Choice is the highest-probability option for Choice questions.
	Choice string `json:"choice,omitempty"`
	// Score is the probability-weighted level index for Score questions.
	Score float64 `json:"score,omitempty"`
	// Confidence is derived from the spread of Probabilities, in [0, 1].
	Confidence float64 `json:"confidence,omitempty"`
	// Probabilities holds the full distribution for Choice (keyed by option)
	// and Score (keyed by level index) questions.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Legend maps Score level indexes back to their descriptions.
	Legend map[string]string `json:"legend,omitempty"`
}

// Usage reports token consumption for a request.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is the System One response body.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}
