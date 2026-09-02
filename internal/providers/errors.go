package providers

import "errors"

// Known error codes that make a failure fallback-worthy even though it is
// not retryable against the same target — see IsFallbackWorthy.
const (
	ErrCodeContextLengthExceeded = "context_length_exceeded"
	ErrCodeModelNotFound         = "model_not_found"
)

// Error is a provider call failure. Retryable failures (e.g. upstream 429 or
// 5xx) let the router fall back to the next target; non-retryable failures
// (e.g. a bad request) abort — UNLESS Code names a known fallback-worthy
// reason (see IsFallbackWorthy), in which case the router still tries the
// next tier even though retrying the SAME target would not help.
type Error struct {
	Status    int
	Retryable bool
	Code      string
	Message   string
}

func (e *Error) Error() string { return e.Message }

// IsRetryable reports whether err is a retryable provider Error.
func IsRetryable(err error) bool {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	return false
}

// IsFallbackWorthy reports whether the router should try the next
// priority tier after this error, rather than aborting the whole request.
// True for every retryable error (unchanged meaning), plus a short list of
// known error codes that mean "this specific target can't serve this
// request" (e.g. its context window is too small, or its model was
// removed) rather than "this request is malformed."
func IsFallbackWorthy(err error) bool {
	if IsRetryable(err) {
		return true
	}
	var pe *Error
	if errors.As(err, &pe) {
		switch pe.Code {
		case ErrCodeContextLengthExceeded, ErrCodeModelNotFound:
			return true
		}
	}
	return false
}
