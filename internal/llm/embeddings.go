package llm

// EmbeddingsRequest is the provider-neutral form of an embeddings call: one
// or more input texts to embed with the given model.
type EmbeddingsRequest struct {
	Model string
	Input []string
	// Dimensions is the requested output vector length, for models that
	// support truncating it (OpenAI's text-embedding-3 family, among
	// others). Nil means the model's own default length.
	Dimensions *int
	// EncodingFormat is the client-facing wire shape for each vector:
	// "float" (a JSON number array) or "base64" (a base64 string of
	// little-endian float32 bytes — what the official OpenAI SDKs request
	// by default). It never reaches an upstream call: providers always
	// decode a plain float array from upstream regardless of what the
	// gateway's own client asked for; only MarshalEmbeddingsResponse reads
	// it. Always normalized to one of the two values, never empty.
	EncodingFormat string
}

// EmbeddingsResponse is what a provider's embeddings call returns: one
// vector per input, in the same order as EmbeddingsRequest.Input, the model
// that actually answered, and the prompt token count the vendor billed —
// embeddings have no completion tokens, so there is no Usage here.
type EmbeddingsResponse struct {
	Model        string
	Data         [][]float64
	PromptTokens int
}
