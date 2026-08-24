package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// defaultAdminPassword is a bootstrap credential only. A server initialized
// from it is placed in must-change mode: every management API except password
// rotation and logout remains blocked, and the value cannot be selected again
// as the replacement password.
const defaultAdminPassword = "admin888"

type loginAttempt struct {
	Failures                 int
	WindowStart, LockedUntil time.Time
}

func adminPasswordPath() string {
	if p := strings.TrimSpace(os.Getenv("M365_ADMIN_PASSWORD_HASH_FILE")); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("M365_TOKEN_CACHE")); p != "" {
		return filepath.Join(filepath.Dir(p), "admin-password.hash")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "m365-gateway", "admin-password.hash")
}

func bootstrapAdminPassword() (string, error) {
	path := firstNonEmptySetting(os.Getenv("M365_ADMIN_BOOTSTRAP_PASSWORD_FILE"), os.Getenv("M365_ADMIN_PASSWORD_FILE"))
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read administrator bootstrap password: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if p := strings.TrimSpace(os.Getenv("M365_ADMIN_PASSWORD")); p != "" {
		return p, nil
	}
	return "", nil
}

func loadAdminPassword() (string, bool, error) {
	// The writable hash always wins over the read-only bootstrap secret.
	if b, err := os.ReadFile(adminPasswordPath()); err == nil {
		hash := strings.TrimSpace(string(b))
		if _, err := bcrypt.Cost([]byte(hash)); err != nil {
			return "", false, fmt.Errorf("administrator password hash is invalid: %w", err)
		}
		// The bootstrap password is intentionally public. Keep the server in
		// must-change mode across restarts until the administrator replaces it;
		// otherwise restarting immediately after first boot would silently bypass
		// the initial-password gate.
		return hash, verifyAdminPassword(hash, defaultAdminPassword), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("read administrator password hash: %w", err)
	}
	bootstrap, err := bootstrapAdminPassword()
	if err != nil {
		return "", false, err
	}
	if bootstrap == "" {
		return "", false, errors.New("administrator password is not configured")
	}
	mustChange := bootstrap == defaultAdminPassword
	hash, err := hashAdminPassword(bootstrap)
	if err != nil {
		return "", false, err
	}
	if err := atomicWriteFile(adminPasswordPath(), []byte(hash+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("persist administrator password hash: %w", err)
	}
	return hash, mustChange, nil
}

func hashAdminPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

func verifyAdminPassword(hash, password string) bool {
	if hash == "" || password == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func saveAdminPassword(password string) (string, error) {
	hash, err := hashAdminPassword(password)
	if err != nil {
		return "", err
	}
	if err := atomicWriteFile(adminPasswordPath(), []byte(hash+"\n"), 0o600); err != nil {
		return "", err
	}
	return hash, nil
}
func clientIP(r *http.Request) string {
	// Trust proxy headers only when the direct peer is loopback (normal local reverse-proxy deployment).
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if net.ParseIP(host).IsLoopback() {
		if x := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); x != "" {
			return x
		}
	}
	if host != "" {
		return host
	}
	return r.RemoteAddr
}
func validNewAdminPassword(p string) error {
	if p == defaultAdminPassword {
		return errors.New("new password must not be the default password")
	}
	if len(p) < 12 {
		return errors.New("new password must be at least 12 characters")
	}
	if len(p) > 256 {
		return errors.New("new password is too long")
	}
	return nil
}
func (s *Server) loginAllowed(ip string, now time.Time) (bool, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.loginAttempts[ip]
	if now.Before(a.LockedUntil) {
		return false, time.Until(a.LockedUntil)
	}
	if a.WindowStart.IsZero() || now.Sub(a.WindowStart) > 15*time.Minute {
		delete(s.loginAttempts, ip)
	}
	return true, 0
}
func (s *Server) recordLoginFailure(ip string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.loginAttempts[ip]
	if a.WindowStart.IsZero() || now.Sub(a.WindowStart) > 15*time.Minute {
		a = loginAttempt{WindowStart: now}
	}
	a.Failures++
	if a.Failures >= 5 {
		a.LockedUntil = now.Add(15 * time.Minute)
	}
	s.loginAttempts[ip] = a
}
func (s *Server) clearLoginFailures(ip string) {
	s.mu.Lock()
	delete(s.loginAttempts, ip)
	s.mu.Unlock()
}
func (s *Server) adminChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	if !s.validAdminSession(r) {
		writeOpenAIError(w, 401, "auth_error", "administrator login required")
		return
	}
	var b struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&b) != nil {
		writeOpenAIError(w, 400, "invalid_request_error", "bad json")
		return
	}
	s.mu.Lock()
	current := s.adminPassword
	s.mu.Unlock()
	if !verifyAdminPassword(current, b.Current) {
		writeOpenAIError(w, 401, "auth_error", "current password is invalid")
		return
	}
	if err := validNewAdminPassword(b.New); err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}
	hash, err := saveAdminPassword(b.New)
	if err != nil {
		writeOpenAIError(w, 500, "storage_error", "could not save administrator password")
		return
	}
	s.mu.Lock()
	s.adminPassword = hash
	s.mustChangePassword = false
	s.adminSessions = map[string]time.Time{}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "m365_admin_session", Path: "/", HttpOnly: true, Secure: cookieSecure(r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	jsonOut(w, map[string]any{"status": "password_changed", "reauthenticate": true})
}
