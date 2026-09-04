package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ValidateProviderConfig reports why a provider's structured configuration
// could never serve a request, so the admin API can reject it when it is
// saved rather than letting the operator discover it from a failed request
// hours later. Kinds that store no configuration accept anything.
//
// It exists so the admin API needs no per-kind knowledge of its own: what a
// kind requires is decided here, next to the code that consumes it.
func ValidateProviderConfig(kind string, raw []byte, baseURL string) error {
	if kind != "vertex" {
		return nil
	}
	cfg, err := parseVertexConfig(raw)
	if err != nil {
		return err
	}
	return validateVertexConfig(cfg, baseURL)
}

// vertexConfig is what a Vertex AI provider needs beyond its credential:
// which cloud project is billed, and which location serves the request.
//
// It is stored as structured configuration rather than folded into base_url
// because an assembled endpoint URL cannot be taken apart again — the same
// two values also spell the prediction endpoint a later embeddings capability
// would call, and an operator entering them separately cannot spell the
// project differently in two places.
type vertexConfig struct {
	Project  string `json:"project"`
	Location string `json:"location"`
}

const (
	// vertexGlobalLocation is the one location served by the unprefixed
	// host; every other location has a host of its own.
	vertexGlobalLocation = "global"

	// vertexAPIHost is the model API's host, regional forms of which carry
	// the location as a prefix.
	vertexAPIHost = "aiplatform.googleapis.com"

	// vertexDefaultPublisher qualifies a model id that arrives without one.
	// Vertex addresses models as publisher/model; Gemini's publisher is
	// google, and it is the only publisher this gateway curates.
	vertexDefaultPublisher = "google"
)

// parseVertexConfig reads a provider's stored JSON configuration. Absent or
// empty input is an empty configuration rather than an error: every provider
// row carries '{}' by default, and kinds that need no configuration keep it.
func parseVertexConfig(raw []byte) (vertexConfig, error) {
	var cfg vertexConfig
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return vertexConfig{}, fmt.Errorf("parse vertex config: %w", err)
	}
	cfg.Project = strings.TrimSpace(cfg.Project)
	cfg.Location = strings.TrimSpace(cfg.Location)
	return cfg, nil
}

// validateVertexConfig reports why a Vertex provider could never serve a
// request, so the admin API can reject it on save instead of letting the
// operator find out from a failed request hours later. An explicit base_url
// stands in for the project because it already names a complete endpoint.
func validateVertexConfig(cfg vertexConfig, baseURL string) error {
	if cfg.Project == "" && baseURL == "" {
		return errors.New("vertex provider needs a cloud project (or an explicit base_url)")
	}
	return nil
}

// location resolves the configured location, defaulting to global — the
// deployment style that needs no regional decision.
func (c vertexConfig) location() string {
	if c.Location == "" {
		return vertexGlobalLocation
	}
	return c.Location
}

// vertexBaseURL resolves the root of the OpenAI-compatible chat surface:
// "{host}/v1/projects/{project}/locations/{location}/endpoints/openapi",
// whose host carries the location as a prefix everywhere except global.
//
// An explicitly configured address wins and is used verbatim. That is not
// polish: it is what makes the provider testable against a local stub, and it
// covers a proxy or an endpoint pinned to another API version.
func vertexBaseURL(cfg vertexConfig, explicit string) string {
	if explicit != "" {
		return strings.TrimRight(explicit, "/")
	}
	loc := cfg.location()
	host := vertexAPIHost
	if loc != vertexGlobalLocation {
		host = loc + "-" + vertexAPIHost
	}
	return fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/endpoints/openapi", host, cfg.Project, loc)
}

// normalizeVertexModel qualifies a bare model id with the default publisher.
// Vertex rejects an unqualified id, and dropping the prefix is an easy slip
// to make in an alias target.
//
// Note what this does NOT do: pricing is keyed by the alias target's spelling,
// not by what goes on the wire, so a price row must be spelled the way the
// alias target is. The normalisation is a defence against a failed request,
// not a way to make two spellings price alike.
func normalizeVertexModel(model string) string {
	if model == "" || strings.Contains(model, "/") {
		return model
	}
	return vertexDefaultPublisher + "/" + model
}
