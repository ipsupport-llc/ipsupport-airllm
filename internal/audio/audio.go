// Package audio holds the provider-neutral intermediate representation for
// batch speech-to-text and text-to-speech requests. Unlike internal/llm
// (JSON in, JSON/SSE out), audio's wire shapes are multipart-in and
// binary-out, so it gets its own types rather than extending llm's.
package audio

// TranscriptionRequest is a provider-neutral request to transcribe audio to text.
type TranscriptionRequest struct {
	Model    string
	Audio    []byte
	Filename string // some providers infer format from the extension
	// Language is an optional BCP-47 hint ("en", "en-US"), already in
	// canonical case (see CanonicalLanguage). Each provider adapts it to what
	// its upstream accepts.
	Language string
	// AlternativeLanguages are further BCP-47 languages the caller may be
	// speaking. Passed on by providers that recognise several languages at
	// once and ignored by those that cannot.
	AlternativeLanguages []string
	Prompt               string // optional context/spelling guidance
	// ModelByLanguage is the serving target's recognition model per language
	// (its recognition_models option). Providers that pick a model by
	// language read it; the others transcribe with Model.
	ModelByLanguage map[string]string
}

// TranscriptionResponse is a transcription result.
type TranscriptionResponse struct {
	Text string
	// Language is the BCP-47 language the provider recognised, in canonical
	// case; empty when the provider did not say.
	Language string
	// Confidence is the provider's confidence in the transcript, 0..1. Zero
	// means the provider reported none.
	Confidence      float64
	DurationSeconds float64 // upstream-reported audio duration, for pricing
	// Model is the upstream model that actually transcribed, when the
	// provider chose one other than the request's Model (see
	// TranscriptionRequest.ModelByLanguage). Empty means the request's.
	Model string
}

// SpeechRequest is a request to synthesize speech from text.
type SpeechRequest struct {
	Model          string
	Input          string
	Voice          string
	ResponseFormat string // mp3/wav/opus/...; provider default if empty
}

// SpeechResponse is synthesized audio.
type SpeechResponse struct {
	Audio       []byte
	ContentType string // e.g. "audio/mpeg", from the upstream response
}
