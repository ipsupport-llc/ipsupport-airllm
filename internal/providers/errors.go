package providers

import "errors"

// Error is a provider call failure. Retryable failures (e.g. upstream 429 or
// 5xx) let the router fall back to the next target; non-retryable failures
// (e.g. a bad request) abort.
type Error struct {
	Status    int
	Retryable bool
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
