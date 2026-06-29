package httpapi

import "testing"

func TestClampHours(t *testing.T) {
	cases := map[string]int{
		"":    24, // default
		"0":   24, // non-positive -> default
		"-5":  24,
		"48":  48,
		"999": 168, // capped at 7 days
		"abc": 24,  // unparseable -> default
	}
	for in, want := range cases {
		if got := clampHours(in); got != want {
			t.Errorf("clampHours(%q) = %d, want %d", in, got, want)
		}
	}
}
