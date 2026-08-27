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
	Language string // optional ISO-639-1 hint
	Prompt   string // optional context/spelling guidance
}

// TranscriptionResponse is a transcription result.
type TranscriptionResponse struct {
	Text            string
	DurationSeconds float64 // upstream-reported audio duration, for pricing
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
