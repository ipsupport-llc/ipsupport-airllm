// Package policy models the access policy carried by an API key: which
// models it may use, whether it may address providers explicitly, and its
// usage limits. A key snapshots its owner's role policy at issue time.
package policy

import "encoding/json"

// KeyPolicy is the effective policy attached to an API key.
type KeyPolicy struct {
	AllowedModels    []string        `json:"allowed_models"`
	AllowPassthrough bool            `json:"allow_passthrough"`
	Limits           json.RawMessage `json:"limits,omitempty"`
}

// Parse decodes a policy snapshot; an empty or invalid snapshot yields a
// zero policy (which permits nothing).
func Parse(raw []byte) KeyPolicy {
	var p KeyPolicy
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	return p
}

// Allows reports whether the policy permits the given model. "*" in the
// allow list permits any model.
func (p KeyPolicy) Allows(model string) bool {
	for _, m := range p.AllowedModels {
		if m == "*" || m == model {
			return true
		}
	}
	return false
}
