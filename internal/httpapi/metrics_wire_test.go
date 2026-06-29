package httpapi

import "testing"

func TestIngressOf(t *testing.T) {
	cases := map[string]string{
		"/v1/chat/completions": "openai",
		"/v1/models":           "openai",
		"/v1/messages":         "anthropic",
		"/api/usage":           "control",
		"/healthz":             "control",
	}
	for path, want := range cases {
		if got := ingressOf(path); got != want {
			t.Errorf("ingressOf(%q) = %q, want %q", path, got, want)
		}
	}
}
