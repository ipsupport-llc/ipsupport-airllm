package policy

import "testing"

func TestAllows(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		model string
		want  bool
	}{
		{"explicit match", `{"allowed_models":["a","b"]}`, "a", true},
		{"explicit miss", `{"allowed_models":["a","b"]}`, "c", false},
		{"wildcard", `{"allowed_models":["*"]}`, "anything", true},
		{"empty denies", `{}`, "a", false},
		{"nil denies", ``, "a", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Parse([]byte(c.raw))
			if got := p.Allows(c.model); got != c.want {
				t.Errorf("Allows(%q) = %v, want %v", c.model, got, c.want)
			}
		})
	}
}

func TestPassthroughFlag(t *testing.T) {
	if Parse([]byte(`{"allow_passthrough":true}`)).AllowPassthrough != true {
		t.Error("expected passthrough true")
	}
	if Parse([]byte(`{}`)).AllowPassthrough != false {
		t.Error("expected passthrough false by default")
	}
}
