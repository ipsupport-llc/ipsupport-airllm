package httpapi

import "net/http"

// handleLogin validates username/password (mock) and sets a session cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	p, ok := s.login.Login(body.Username, body.Password)
	if !ok {
		writeControlError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	s.login.SetSession(w, p)
	writeJSON(w, http.StatusOK, map[string]any{
		"subject":  p.Subject,
		"roles":    p.Roles,
		"is_admin": p.IsAdmin(),
	})
}

// handleLogout clears the session cookie.
func (s *Server) handleLogout(w http.ResponseWriter, _ *http.Request) {
	s.login.ClearSession(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}
