package providers

import (
	"errors"
	"testing"
)

func TestIsFallbackWorthyRetryable(t *testing.T) {
	err := &Error{Status: 503, Retryable: true}
	if !IsFallbackWorthy(err) {
		t.Error("a retryable error must be fallback-worthy")
	}
}

func TestIsFallbackWorthyKnownCode(t *testing.T) {
	for _, code := range []string{ErrCodeContextLengthExceeded, ErrCodeModelNotFound} {
		err := &Error{Status: 400, Retryable: false, Code: code}
		if !IsFallbackWorthy(err) {
			t.Errorf("code %q must be fallback-worthy even though Retryable=false", code)
		}
	}
}

func TestIsFallbackWorthyUnknownCode(t *testing.T) {
	err := &Error{Status: 400, Retryable: false, Code: ""}
	if IsFallbackWorthy(err) {
		t.Error("a non-retryable error with no recognized code must not be fallback-worthy")
	}
	err2 := &Error{Status: 400, Retryable: false, Code: "some_other_code"}
	if IsFallbackWorthy(err2) {
		t.Error("a non-retryable error with an unrecognized code must not be fallback-worthy")
	}
}

func TestIsFallbackWorthyNonProviderError(t *testing.T) {
	if IsFallbackWorthy(errors.New("plain error")) {
		t.Error("a plain non-*Error must not be fallback-worthy, matching IsRetryable's existing behavior")
	}
}
