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

// Limits are per-window usage caps. Keys are window names ("5h"/"24h"/"7d");
// a missing or non-positive value means no cap for that window/unit.
type Limits struct {
	Tokens  map[string]int64   `json:"tokens,omitempty"`
	CostUSD map[string]float64 `json:"cost_usd,omitempty"`
}

// ParseLimits decodes the limits snapshot.
func (p KeyPolicy) ParseLimits() Limits {
	var l Limits
	if len(p.Limits) > 0 {
		_ = json.Unmarshal(p.Limits, &l)
	}
	return l
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
