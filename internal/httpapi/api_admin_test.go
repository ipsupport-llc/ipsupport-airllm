package httpapi

import (
	"encoding/json"
	"testing"
)

// TestProviderConfigToStore covers the two decisions a provider save makes
// about structured configuration: which value to persist when the request
// omits one, and whether the result could ever serve a request.
func TestProviderConfigToStore(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		supplied string
		stored   string
		baseURL  string
		want     string
		wantErr  bool
	}{
		{"a kind that needs no config gets the column default", "openai", "", "", "", "{}", false},
		{"an explicit null is the column default too", "openai", "null", "", "", "{}", false},
		{"an unvalidated kind keeps whatever it was given", "openai", `{"anything":1}`, "", "", `{"anything":1}`, false},
		{"vertex with a project", "vertex", `{"project":"acme","location":"us-west1"}`, "", "", `{"project":"acme","location":"us-west1"}`, false},
		{"a supplied config replaces the stored one", "vertex", `{"project":"new"}`, `{"project":"old"}`, "", `{"project":"new"}`, false},
		{"an omitted config keeps the stored one", "vertex", "", `{"project":"acme","location":"global"}`, "", `{"project":"acme","location":"global"}`, false},
		{"vertex with only an explicit address", "vertex", "{}", "", "http://127.0.0.1:8080", "{}", false},
		{"vertex with neither is rejected on save", "vertex", "{}", "", "", "", true},
		{"vertex with nothing anywhere is rejected on save", "vertex", "", "", "", "", true},
		{"vertex with a malformed config is rejected on save", "vertex", `{"project":`, "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := providerConfigToStore(c.kind, json.RawMessage(c.supplied), c.stored, c.baseURL)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("providerConfigToStore: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
