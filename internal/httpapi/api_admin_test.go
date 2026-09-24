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

// TestCredentialToStore covers what a provider save does to the stored
// credential. The asymmetry is the point: a save that says nothing keeps the
// stored credential, so only an explicit clear can remove one, and a request
// cannot both set and clear.
func TestCredentialToStore(t *testing.T) {
	cases := []struct {
		name       string
		apiKey     string
		credJSON   string
		clear      bool
		wantSecret string
		wantChange credentialChange
		wantErr    bool
	}{
		{"a save that says nothing keeps the stored credential", "", "", false, "", credentialKept, false},
		{"an api key replaces it", "sk-1", "", false, "sk-1", credentialSet, false},
		{"a service-account key replaces it", "", `{"type":"service_account"}`, false, `{"type":"service_account"}`, credentialSet, false},
		{"an explicit clear removes it", "", "", true, "", credentialCleared, false},
		{"both credential fields are rejected", "sk-1", "{}", false, "", "", true},
		{"clearing while setting an api key is rejected", "sk-1", "", true, "", "", true},
		{"clearing while setting a service-account key is rejected", "", "{}", true, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			secret, change, err := credentialToStore(c.apiKey, c.credJSON, c.clear)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %q (%s)", secret, change)
				}
				return
			}
			if err != nil {
				t.Fatalf("credentialToStore: %v", err)
			}
			if secret != c.wantSecret || change != c.wantChange {
				t.Errorf("got (%q, %s), want (%q, %s)", secret, change, c.wantSecret, c.wantChange)
			}
		})
	}
}
