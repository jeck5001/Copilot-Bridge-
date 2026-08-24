package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vipamess/Copilot-Bridge-/internal/proxy"
)

type AccountToken struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	DisplayName  string    `json:"displayName,omitempty"`
	Status       string    `json:"status"`
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	OID          string    `json:"oid,omitempty"`
	TID          string    `json:"tid,omitempty"`
	ClientID     string    `json:"clientId,omitempty"`
	Proxy        string    `json:"proxy,omitempty"`
}

type Cache struct {
	Accounts []AccountToken `json:"accounts"`
}

type Store struct {
	mu       sync.Mutex
	path     string
	data     Cache
	inflight map[string]*inflightRefresh
	// refreshBackoff records the earliest retry time per account after a
	// failed token refresh. Without it, a dead refresh token would be
	// redeemed again on every incoming request, hammering the AAD token
	// endpoint and risking tenant throttling.
	refreshBackoff map[string]time.Time
}

// refreshFailureBackoff is how long a failed refresh blocks further refresh
// attempts for that account. EnsureValid fails fast during the backoff window
// so callers can fail over to a healthy account immediately.
const refreshFailureBackoff = 5 * time.Minute

// RefreshDue proactively refreshes every account whose access token expires
// within `window`. It reuses the same single-flight coalescing as EnsureValid.
// Returned errors are keyed by account ID so callers can feed their own health
// tracking; successful refreshes keep sessions pinned to those accounts alive
// without paying AAD latency on the request path.
func (s *Store) RefreshDue(window time.Duration) map[string]error {
	out := map[string]error{}
	now := time.Now()
	for _, acc := range s.List() {
		if acc.RefreshToken == "" {
			continue
		}
		if now.Before(acc.ExpiresAt.Add(-window)) {
			continue
		}
		_, err := s.refreshInflight(acc)
		out[acc.ID] = err
	}
	return out
}

// inflightRefresh coalesces concurrent EnsureValid refreshes for the same
// account: an AAD refresh token can only be redeemed once, so a stampede of
// concurrent requests must not each call Refresh(). Waiters block on the shared
// flight and receive the winner's outcome.
type inflightRefresh struct {
	done chan struct{}
	acc  AccountToken
	err  error
}

func CachePath() string {
	if p := os.Getenv("M365_TOKEN_CACHE"); p != "" {
		return p
	}
	if p := os.Getenv("M365_TOKEN_FILE"); p != "" {
		return p
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return filepath.Join(".", ".config", "m365-gateway", "accounts.json")
	}
	return filepath.Join(h, ".config", "m365-gateway", "accounts.json")
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		path = CachePath()
	}
	s := &Store{path: path, data: Cache{Accounts: []AccountToken{}}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, err
	}
	// Normalize oid/tid for older cache entries.
	for i := range s.data.Accounts {
		a := &s.data.Accounts[i]
		if a.OID == "" {
			a.OID = a.ID
		}
		if a.ID == "" {
			a.ID = a.OID
		}
	}
	return s, nil
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) saveDataLocked(data Cache) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.path, b, 0o600)
}

// atomicWrite writes to a temp file then renames, so a crash mid-write never
// leaves a truncated token cache that would force every account to re-auth.
func atomicWrite(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".m365-token-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	ok = true
	return nil
}

func cloneCache(data Cache) Cache {
	return Cache{Accounts: append([]AccountToken(nil), data.Accounts...)}
}

func (s *Store) List() []AccountToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AccountToken, len(s.data.Accounts))
	copy(out, s.data.Accounts)
	return out
}

func (s *Store) Upsert(tok TokenSet) (AccountToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := tok.HomeOID
	if id == "" {
		id = tok.Email
	}
	if id == "" {
		id = "account-" + time.Now().Format("150405")
	}
	acc := AccountToken{
		ID:           id,
		Email:        tok.Email,
		DisplayName:  tok.DisplayName,
		Status:       "online",
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.ExpiresAt,
		UpdatedAt:    time.Now(),
		OID:          firstNonEmpty(tok.HomeOID, id),
		TID:          tok.TenantID,
		ClientID:     ClientID(),
	}
	next := cloneCache(s.data)
	found := false
	for i, existing := range next.Accounts {
		if existing.ID == acc.ID || (acc.Email != "" && existing.Email == acc.Email) {
			if acc.RefreshToken == "" {
				acc.RefreshToken = existing.RefreshToken
			}
			if acc.TID == "" {
				acc.TID = existing.TID
			}
			if acc.OID == "" {
				acc.OID = existing.OID
			}
			// OAuth 登录流程不带 proxy, 保留用户在 Web 界面手动设置的代理
			if acc.Proxy == "" {
				acc.Proxy = existing.Proxy
			}
			next.Accounts[i] = acc
			found = true
			break
		}
	}
	if !found {
		next.Accounts = append(next.Accounts, acc)
	}
	if err := s.saveDataLocked(next); err != nil {
		return AccountToken{}, err
	}
	s.data = next
	return acc, nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneCache(s.data)
	next.Accounts = next.Accounts[:0]
	for _, a := range s.data.Accounts {
		if a.ID != id {
			next.Accounts = append(next.Accounts, a)
		}
	}
	if err := s.saveDataLocked(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

func (s *Store) SetProxy(id, proxy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Accounts {
		if s.data.Accounts[i].ID == id {
			next := cloneCache(s.data)
			next.Accounts[i].Proxy = proxy
			if err := s.saveDataLocked(next); err != nil {
				return err
			}
			s.data = next
			return nil
		}
	}
	return os.ErrNotExist
}

func (s *Store) Get(id string) (AccountToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.data.Accounts {
		if a.ID == id || a.OID == id || a.Email == id {
			return a, true
		}
	}
	return AccountToken{}, false
}

func (s *Store) First() (AccountToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Accounts) == 0 {
		return AccountToken{}, false
	}
	return s.data.Accounts[0], true
}

func (s *Store) EnsureValid(id string) (AccountToken, error) {
	acc, ok := s.Get(id)
	if !ok {
		return AccountToken{}, os.ErrNotExist
	}
	if time.Now().Before(acc.ExpiresAt.Add(-30 * time.Second)) {
		return acc, nil
	}
	if acc.RefreshToken == "" {
		s.mu.Lock()
		next := cloneCache(s.data)
		for i, a := range s.data.Accounts {
			if a.ID == acc.ID {
				acc.Status = "expired"
				next.Accounts[i] = acc
				if err := s.saveDataLocked(next); err == nil {
					s.data = next
				}
				break
			}
		}
		s.mu.Unlock()
		return acc, fmtExpired()
	}
	return s.refreshInflight(acc)
}

// refreshInflight runs the AAD token refresh exactly once per account; waiters
// block on the shared flight instead of each redeeming the one-time refresh
// token (which would otherwise fail a concurrent stampede with 401s). The
// winner's outcome is broadcast to all waiters. A failed refresh starts a
// backoff window during which further attempts fail fast.
func (s *Store) refreshInflight(acc AccountToken) (AccountToken, error) {
	s.mu.Lock()
	// Fail fast while a previous failure is in its backoff window.
	if until, ok := s.refreshBackoff[acc.ID]; ok {
		if time.Now().Before(until) {
			s.mu.Unlock()
			return acc, fmt.Errorf("token refresh backing off for account %s until %s", acc.ID, until.Format(time.RFC3339))
		}
		delete(s.refreshBackoff, acc.ID)
	}
	// Re-read the authoritative state while holding the same lock used to
	// publish refresh results. A caller can arrive with a stale snapshot just
	// after the previous flight was removed; returning the newly refreshed token
	// here prevents a second redemption of a one-time refresh token.
	for _, current := range s.data.Accounts {
		if current.ID != acc.ID {
			continue
		}
		if time.Now().Before(current.ExpiresAt.Add(-30 * time.Second)) {
			s.mu.Unlock()
			return current, nil
		}
		acc = current
		break
	}
	if s.inflight == nil {
		s.inflight = map[string]*inflightRefresh{}
	}
	if f, ok := s.inflight[acc.ID]; ok {
		s.mu.Unlock()
		<-f.done
		return f.acc, f.err
	}
	f := &inflightRefresh{done: make(chan struct{})}
	s.inflight[acc.ID] = f
	s.mu.Unlock()

	client, err := proxy.HTTPClientFor(acc.Proxy)
	if err != nil {
		f.acc, f.err = acc, err
		close(f.done)
		s.mu.Lock()
		delete(s.inflight, acc.ID)
		s.mu.Unlock()
		return acc, err
	}
	tok, err := Refresh(acc.RefreshToken, client)
	if err != nil {
		acc.Status = "expired"
		s.mu.Lock()
		if s.refreshBackoff == nil {
			s.refreshBackoff = map[string]time.Time{}
		}
		s.refreshBackoff[acc.ID] = time.Now().Add(refreshFailureBackoff)
		next := cloneCache(s.data)
		for i, a := range s.data.Accounts {
			if a.ID == acc.ID {
				next.Accounts[i] = acc
				if saveErr := s.saveDataLocked(next); saveErr == nil {
					s.data = next
				} else {
					err = fmt.Errorf("%w (also failed to persist expired status: %v)", err, saveErr)
				}
				break
			}
		}
		s.mu.Unlock()
		f.acc, f.err = acc, err
		close(f.done)
		s.mu.Lock()
		delete(s.inflight, acc.ID)
		s.mu.Unlock()
		return acc, err
	}
	if tok.Email == "" {
		tok.Email = acc.Email
	}
	if tok.DisplayName == "" {
		tok.DisplayName = acc.DisplayName
	}
	if tok.HomeOID == "" {
		tok.HomeOID = firstNonEmpty(acc.OID, acc.ID)
	}
	if tok.TenantID == "" {
		tok.TenantID = acc.TID
	}
	f.acc, f.err = s.Upsert(tok)
	s.mu.Lock()
	if s.refreshBackoff != nil {
		delete(s.refreshBackoff, acc.ID)
	}
	s.mu.Unlock()
	close(f.done)
	s.mu.Lock()
	delete(s.inflight, acc.ID)
	s.mu.Unlock()
	return f.acc, f.err
}

func fmtExpired() error {
	return errors.New("token_expired: refresh token missing or expired")
}
