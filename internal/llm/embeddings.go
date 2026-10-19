package llm

// EmbeddingsRequest is the provider-neutral form of an embeddings call: one
// or more input texts to embed with the given model.
type EmbeddingsRequest struct {
	Model string
	Input []string
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
