package auth

import "testing"

func TestHashAndCheckPassword(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "correct horse battery") {
		t.Error("correct password must verify")
	}
	if CheckPassword(h, "wrong") {
		t.Error("wrong password must not verify")
	}
	if CheckPassword("", "anything") {
		t.Error("empty hash must never verify (no local password)")
	}
}
