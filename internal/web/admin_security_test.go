package web

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func adminTestClient(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	ts := httptest.NewTLSServer(h)
	jar, _ := cookiejar.New(nil)
	c := ts.Client()
	c.Jar = jar
	t.Cleanup(ts.Close)
	return ts, c
}

func postJSON(t *testing.T, c *http.Client, url, body string) *http.Response {
	t.Helper()
	r, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDefaultPasswordForcesChangeAndRotatesSessions(t *testing.T) {
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_ADMIN_PASSWORD_HASH_FILE", t.TempDir()+"/admin-password.hash")
	t.Setenv("M365_ADMIN_PASSWORD", defaultAdminPassword)
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, c := adminTestClient(t, s.Routes())

	r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"admin888"}`)
	if r.StatusCode != 200 {
		t.Fatalf("login=%d", r.StatusCode)
	}
	var login map[string]any
	_ = json.NewDecoder(r.Body).Decode(&login)
	r.Body.Close()
	if login["must_change_password"] != true {
		t.Fatalf("login=%#v", login)
	}

	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("protected status=%d", r.StatusCode)
	}

	r = postJSON(t, c, ts.URL+"/api/admin/change-password", `{"current_password":"admin888","new_password":"a-new-password-123"}`)
	if r.StatusCode != 200 {
		t.Fatalf("change=%d", r.StatusCode)
	}
	r.Body.Close()

	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session status=%d", r.StatusCode)
	}

	r = postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"a-new-password-123"}`)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("new login=%d", r.StatusCode)
	}
	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("new session status=%d", r.StatusCode)
	}
}

func TestConsoleScriptIsPublicSoLoginCanInitialize(t *testing.T) {
	s := &Server{}
	called := false
	handler := s.adminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin.js", nil))
	if !called || w.Code != http.StatusNoContent {
		t.Fatalf("admin.js blocked before login: called=%v status=%d", called, w.Code)
	}
}

func TestAdminLoginLocksAfterFiveFailures(t *testing.T) {
	t.Setenv("M365_ADMIN_PASSWORD", "correct-password")
	t.Setenv("M365_ADMIN_PASSWORD_HASH_FILE", t.TempDir()+"/admin-password.hash")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, c := adminTestClient(t, s.Routes())
	for i := 0; i < 5; i++ {
		r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"wrong"}`)
		r.Body.Close()
		if r.StatusCode != 401 {
			t.Fatalf("attempt %d=%d", i+1, r.StatusCode)
		}
	}
	r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"correct-password"}`)
	defer r.Body.Close()
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("locked=%d retry=%q", r.StatusCode, r.Header.Get("Retry-After"))
	}
}

func TestPersistedPasswordOverridesBootstrapEnv(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/admin-password.hash"
	bootstrap := dir + "/bootstrap"
	t.Setenv("M365_ADMIN_PASSWORD_HASH_FILE", path)
	t.Setenv("M365_ADMIN_BOOTSTRAP_PASSWORD_FILE", bootstrap)
	t.Setenv("M365_ADMIN_PASSWORD", "old-bootstrap-password")
	if err := os.WriteFile(bootstrap, []byte("old-bootstrap-password"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := saveAdminPassword("persisted-new-password"); err != nil {
		t.Fatal(err)
	}
	got, mustChange, err := loadAdminPassword()
	if err != nil || !verifyAdminPassword(got, "persisted-new-password") || mustChange {
		t.Fatalf("hash valid=%v mustChange=%v err=%v", verifyAdminPassword(got, "persisted-new-password"), mustChange, err)
	}
}

func TestPersistedDefaultPasswordStillForcesChangeAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/admin-password.hash"
	t.Setenv("M365_ADMIN_PASSWORD_HASH_FILE", path)
	t.Setenv("M365_ADMIN_PASSWORD", defaultAdminPassword)

	firstHash, firstMustChange, err := loadAdminPassword()
	if err != nil || !firstMustChange || !verifyAdminPassword(firstHash, defaultAdminPassword) {
		t.Fatalf("first load valid=%v mustChange=%v err=%v", verifyAdminPassword(firstHash, defaultAdminPassword), firstMustChange, err)
	}
	secondHash, secondMustChange, err := loadAdminPassword()
	if err != nil || !secondMustChange || !verifyAdminPassword(secondHash, defaultAdminPassword) {
		t.Fatalf("restart load valid=%v mustChange=%v err=%v", verifyAdminPassword(secondHash, defaultAdminPassword), secondMustChange, err)
	}
}

func TestMissingAdminPasswordFailsClosed(t *testing.T) {
	t.Setenv("M365_ADMIN_PASSWORD_HASH_FILE", t.TempDir()+"/missing.hash")
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", "")
	t.Setenv("M365_ADMIN_BOOTSTRAP_PASSWORD_FILE", "")
	if _, _, err := loadAdminPassword(); err == nil {
		t.Fatal("missing password must fail closed")
	}
}

func TestExpiredLoginWindowResets(t *testing.T) {
	s := &Server{loginAttempts: map[string]loginAttempt{"x": {Failures: 4, WindowStart: time.Now().Add(-16 * time.Minute)}}}
	if ok, _ := s.loginAllowed("x", time.Now()); !ok {
		t.Fatal("expired window remained locked")
	}
}
