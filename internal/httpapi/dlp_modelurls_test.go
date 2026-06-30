package httpapi

import "testing"

func TestEffectiveModelURLs(t *testing.T) {
	cases := []struct {
		name string
		cfg  dlpConfig
		want []string
	}{
		{"list wins", dlpConfig{ModelURLs: []string{"http://a:8000", "http://b:8000"}, ModelURL: "http://old:8000"}, []string{"http://a:8000", "http://b:8000"}},
		{"fallback to single", dlpConfig{ModelURL: "http://old:8000"}, []string{"http://old:8000"}},
		{"blanks dropped", dlpConfig{ModelURLs: []string{" ", "http://a:8000", ""}}, []string{"http://a:8000"}},
		{"all empty list falls back", dlpConfig{ModelURLs: []string{"", "  "}, ModelURL: "http://old:8000"}, []string{"http://old:8000"}},
		{"nothing", dlpConfig{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.effectiveModelURLs()
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
