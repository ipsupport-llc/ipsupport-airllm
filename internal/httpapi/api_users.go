package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
)

const minPasswordLen = 8

func validatePassword(pw string) error {
	if len(pw) < minPasswordLen {
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	return nil
}

func validateNewUser(username string, roles []string, password string, known map[string]bool) error {
	if username == "" {
		return errors.New("username is required")
	}
	if err := validatePassword(password); err != nil {
		return err
	}
	for _, r := range roles {
		if !known[r] {
			return fmt.Errorf("unknown role %q", r)
		}
	}
	return nil
}

// knownRoles loads role keys from roles_policy for validation.
func (s *Server) knownRoles(r *http.Request) (map[string]bool, error) {
	rows, err := s.st.PG.Query(r.Context(), `SELECT role FROM roles_policy`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, err
		}
		out[role] = true
	}
	return out, rows.Err()
}

func (s *Server) users() *store.PGUsers { return store.NewPGUsers(s.st) }

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	var body struct {
		Username string   `json:"username"`
		Email    string   `json:"email"`
		Display  string   `json:"display"`
		Roles    []string `json:"roles"`
		Password string   `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	known, err := s.knownRoles(r)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load roles")
		return
	}
	if err := validateNewUser(body.Username, body.Roles, body.Password, known); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "hash failed")
		return
	}
	id, err := s.users().CreateLocal(r.Context(), auth.UserRow{
		Subject: body.Username, Email: body.Email, Display: body.Display, Roles: body.Roles, PasswordHash: hash,
	})
	if err != nil {
		writeControlError(w, http.StatusBadRequest, "create failed (username taken?)")
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "user.create", body.Username, map[string]any{"roles": body.Roles})
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")
	var body struct {
		Email    string   `json:"email"`
		Display  string   `json:"display"`
		Roles    []string `json:"roles"`
		Disabled bool     `json:"disabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := s.guardLastAdmin(r, id, body.Roles, body.Disabled); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.users().Update(r.Context(), id, body.Email, body.Display, body.Roles, body.Disabled); err != nil {
		s.writeUserErr(w, err)
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "user.update", id, map[string]any{"disabled": body.Disabled, "roles": body.Roles})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := validatePassword(body.Password); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, _ := auth.HashPassword(body.Password)
	if err := s.users().SetPassword(r.Context(), id, hash); err != nil {
		s.writeUserErr(w, err)
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "user.password_reset", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	id := r.PathValue("id")
	if err := s.guardLastAdmin(r, id, nil, true); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := s.users().KeyCount(r.Context(), id)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to check keys")
		return
	}
	if n > 0 {
		writeControlError(w, http.StatusBadRequest, "user still owns active API keys; revoke them first")
		return
	}
	if err := s.users().Delete(r.Context(), id); err != nil {
		s.writeUserErr(w, err)
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "user.delete", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleChangeOwnPassword lets a local user change their own password.
func (s *Server) handleChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	var body struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeControlError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := validatePassword(body.New); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.users().ByID(r.Context(), sess.userID)
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, "failed to load user")
		return
	}
	if u.AuthSource != "local" || !auth.CheckPassword(u.PasswordHash, body.Current) {
		writeControlError(w, http.StatusBadRequest, "current password is incorrect")
		return
	}
	hash, _ := auth.HashPassword(body.New)
	if err := s.users().SetPassword(r.Context(), sess.userID, hash); err != nil {
		s.writeUserErr(w, err)
		return
	}
	s.audit(r.Context(), sess.principal.Subject, "user.self_password", sess.userID, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// guardLastAdmin blocks an update/delete that would remove the final admin.
func (s *Server) guardLastAdmin(r *http.Request, id string, newRoles []string, disabling bool) error {
	u, err := s.users().ByID(r.Context(), id)
	if err != nil {
		return nil // not found -> let the underlying op report it
	}
	wasAdmin := false
	for _, role := range u.Roles {
		if role == auth.AdminRole {
			wasAdmin = true
		}
	}
	if !wasAdmin {
		return nil
	}
	stillAdmin := false
	for _, role := range newRoles {
		if role == auth.AdminRole {
			stillAdmin = true
		}
	}
	if disabling || !stillAdmin {
		n, _ := s.users().CountAdmins(r.Context())
		if n <= 1 {
			return errors.New("cannot remove or disable the last admin")
		}
	}
	return nil
}

func (s *Server) writeUserErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrUserNotFound) {
		writeControlError(w, http.StatusNotFound, "user not found")
		return
	}
	writeControlError(w, http.StatusInternalServerError, "operation failed")
}
