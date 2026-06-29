package httpapi

import "testing"

func TestValidateNewUser(t *testing.T) {
	known := map[string]bool{"airllm_admin": true, "airllm_user": true}
	if err := validateNewUser("alice", []string{"airllm_user"}, "longenough", known); err != nil {
		t.Errorf("valid user rejected: %v", err)
	}
	if err := validateNewUser("", []string{"airllm_user"}, "longenough", known); err == nil {
		t.Error("empty username must fail")
	}
	if err := validateNewUser("bob", []string{"airllm_user"}, "short", known); err == nil {
		t.Error("short password must fail")
	}
	if err := validateNewUser("bob", []string{"nope"}, "longenough", known); err == nil {
		t.Error("unknown role must fail")
	}
}

func TestValidatePasswordLen(t *testing.T) {
	if err := validatePassword("1234567"); err == nil {
		t.Error("7-char password must fail (<8)")
	}
	if err := validatePassword("12345678"); err != nil {
		t.Errorf("8-char password must pass: %v", err)
	}
}
