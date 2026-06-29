package auth

import "golang.org/x/crypto/bcrypt"

// dummyHash is a valid bcrypt hash of a random value, compared against on
// "user not found" so login timing does not reveal whether a user exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("airllm-timing-dummy"), bcrypt.DefaultCost)

// HashPassword returns a bcrypt hash of pw.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword reports whether pw matches the bcrypt hash. An empty hash
// (no local password set) never matches.
func CheckPassword(hash, pw string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// checkAgainstDummy burns equivalent time on the not-found path.
func checkAgainstDummy(pw string) { _ = bcrypt.CompareHashAndPassword(dummyHash, []byte(pw)) }
