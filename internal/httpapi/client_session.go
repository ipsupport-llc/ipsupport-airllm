package httpapi

import (
	"context"
	"net/http"
)

// clientSessionHeader is the optional request header a client uses to tie
// its requests together (e.g. every turn of one phone call). The gateway
// logs it, records it in the usage ledger and, on an alias with session
// affinity, keeps the session on the tier a fallback moved it to.
const clientSessionHeader = "X-Session-Id"

// maxClientSessionLen bounds what a client can make the gateway log.
const maxClientSessionLen = 128

type clientSessionKey struct{}

// withClientSession carries the request's session header, if any, on ctx.
func withClientSession(ctx context.Context, r *http.Request) context.Context {
	id := r.Header.Get(clientSessionHeader)
	if id == "" {
		return ctx
	}
	if len(id) > maxClientSessionLen {
		id = id[:maxClientSessionLen]
	}
	return context.WithValue(ctx, clientSessionKey{}, id)
}

// clientSessionFrom returns the session withClientSession stored, or "".
func clientSessionFrom(ctx context.Context) string {
	id, _ := ctx.Value(clientSessionKey{}).(string)
	return id
}
