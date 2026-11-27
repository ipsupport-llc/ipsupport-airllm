package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrCodeUpstreamTimeout marks a non-streaming call cut off by the
// provider's own ceiling (UPSTREAM_CHAT_TIMEOUT) while the caller was still
// waiting. Retryable, so it falls through to the next target.
const ErrCodeUpstreamTimeout = "upstream_timeout"

type callBudgetKey struct{}

// WithCallBudget records on ctx the time budget the caller enforces on this
// call itself (a target's timeout_ms). A non-streaming call that carries one
// skips the provider's own ceiling: the budget replaces it, so a target
// given more time than UPSTREAM_CHAT_TIMEOUT gets it. d <= 0 records nothing.
func WithCallBudget(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, callBudgetKey{}, d)
}

// CallBudget is the budget WithCallBudget recorded on ctx, if any.
func CallBudget(ctx context.Context) (time.Duration, bool) {
	d, ok := ctx.Value(callBudgetKey{}).(time.Duration)
	return d, ok && d > 0
}

// chatCeilinger is a provider whose non-streaming Chat has a ceiling of its
// own (SetChatTimeout).
type chatCeilinger interface {
	ChatCeiling() time.Duration
}

// ChatCeiling is the ceiling p's non-streaming Chat applies to a call that
// carries no budget: its own (UPSTREAM_CHAT_TIMEOUT), or the built-in
// default for a provider that has none. A caller that serves a unary
// request some other way uses it to stay as bounded as Chat would be.
func ChatCeiling(p Provider) time.Duration {
	if c, ok := p.(chatCeilinger); ok {
		if d := c.ChatCeiling(); d > 0 {
			return d
		}
	}
	return defaultChatTimeout
}

// withCeiling bounds a non-streaming call by ceiling (defaultChatTimeout
// when <= 0), unless ctx carries a caller-enforced budget (CallBudget), in
// which case ctx is used as it is. The returned duration is the ceiling
// applied, 0 when none was.
func withCeiling(ctx context.Context, ceiling time.Duration) (context.Context, context.CancelFunc, time.Duration) {
	if _, ok := CallBudget(ctx); ok {
		return ctx, func() {}, 0
	}
	if ceiling <= 0 {
		ceiling = defaultChatTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, ceiling)
	return cctx, cancel, ceiling
}

// ceilingError replaces err, the failure of a call made under cctx (from
// withCeiling over parent), with an ErrCodeUpstreamTimeout error when it was
// the ceiling that ran out and the caller is still waiting: the ceiling's
// deadline has passed and err is that deadline (directly or under a
// transport error). Any other error — an upstream answer that happened to
// arrive as the deadline passed, among them — is returned unchanged.
func ceilingError(parent, cctx context.Context, name string, ceiling time.Duration, err error) error {
	if err == nil || ceiling <= 0 || parent.Err() != nil ||
		!errors.Is(cctx.Err(), context.DeadlineExceeded) || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &Error{
		Status:    http.StatusGatewayTimeout,
		Retryable: true,
		Code:      ErrCodeUpstreamTimeout,
		Message:   fmt.Sprintf("upstream %s did not answer within %s (UPSTREAM_CHAT_TIMEOUT): %v", name, ceiling, err),
	}
}
