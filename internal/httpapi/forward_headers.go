package httpapi

import (
	"context"
	"net/http"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
)

// withForwardedHeaders copies the subset of r's headers the operator has
// opted into forwarding (allow, from config.Config.ForwardHeaders) onto
// ctx, for a provider's outbound upstream call to attach (see
// providers.WithForwardedHeaders). A header not in allow is dropped:
// forwarding every client header by default would let a client override
// the gateway's own Content-Type/Authorization/Accept on its upstream
// call, or headers meant only for the gateway itself.
func withForwardedHeaders(ctx context.Context, r *http.Request, allow []string) context.Context {
	if len(allow) == 0 {
		return ctx
	}
	var out http.Header
	for _, name := range allow {
		key := http.CanonicalHeaderKey(name)
		if v, ok := r.Header[key]; ok {
			if out == nil {
				out = http.Header{}
			}
			out[key] = v
		}
	}
	return providers.WithForwardedHeaders(ctx, out)
}
