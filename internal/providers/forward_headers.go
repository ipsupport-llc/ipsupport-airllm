package providers

import (
	"context"
	"net/http"
)

type forwardedHeadersKey struct{}

// WithForwardedHeaders attaches request headers the gateway's operator has
// opted into forwarding to upstream providers (see sendChatCompletions) on
// ctx. An empty header leaves ctx unchanged, mirroring httpapi's
// withClientSession convention.
func WithForwardedHeaders(ctx context.Context, header http.Header) context.Context {
	if len(header) == 0 {
		return ctx
	}
	return context.WithValue(ctx, forwardedHeadersKey{}, header)
}

// ForwardedHeadersFromContext returns the headers WithForwardedHeaders
// attached, or nil. Exported alongside WithForwardedHeaders so httpapi can
// assert what it built without a full round trip to a mock upstream.
func ForwardedHeadersFromContext(ctx context.Context) http.Header {
	h, _ := ctx.Value(forwardedHeadersKey{}).(http.Header)
	return h
}
