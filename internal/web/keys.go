package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type apiKeyRecord struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Hash       string     `json:"hash"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	Revoked    bool       `json:"revoked"`
}
type apiKeyStore struct {
	mu   sync.Mutex
	path string
	Keys []apiKeyRecord `json:"keys"`
}

func openAPIKeys() (*apiKeyStore, error) {
	p := strings.TrimSpace(os.Getenv("M365_API_KEYS"))
	if p == "" {
		h, _ := os.UserHomeDir()
		p = filepath.Join(h, ".config", "m365-gateway", "api-keys.json")
	}
	s := &apiKeyStore{path: p}
	b, e := os.ReadFile(p)
	if e != nil {
		if os.IsNotExist(e) {
			return s, nil
		}
		return nil, e
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *apiKeyStore) saveKeysLocked(keys []apiKeyRecord) error {
	b, err := json.MarshalIndent(struct {
		Keys []apiKeyRecord `json:"keys"`
	}{Keys: keys}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.path, b, 0o600)
}
func keyHash(k string) string { h := sha256.Sum256([]byte(k)); return hex.EncodeToString(h[:]) }
func (s *apiKeyStore) create(name string, days int) (apiKeyRecord, string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return apiKeyRecord{}, "", e
	}
	raw := "m365_" + hex.EncodeToString(b)
	r := apiKeyRecord{ID: hex.EncodeToString(b[:8]), Name: name, Prefix: raw[:12], Hash: keyHash(raw), CreatedAt: time.Now()}
	if days > 0 {
		exp := time.Now().AddDate(0, 0, days)
		r.ExpiresAt = &exp
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append(append([]apiKeyRecord(nil), s.Keys...), r)
	if err := s.saveKeysLocked(next); err != nil {
		return apiKeyRecord{}, "", err
	}
	s.Keys = next
	return r, raw, nil
}
func (s *apiKeyStore) list() []apiKeyRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]apiKeyRecord, len(s.Keys))
	copy(out, s.Keys)
	for i := range out {
		out[i].Hash = ""
	}
	return out
}
func (s *apiKeyStore) revoke(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Keys {
		if s.Keys[i].ID == id && !s.Keys[i].Revoked {
			next := append([]apiKeyRecord(nil), s.Keys...)
			next[i].Revoked = true
			if err := s.saveKeysLocked(next); err != nil {
				return false, err
			}
			s.Keys = next
			return true, nil
		}
	}
	return false, nil
}

// remove permanently deletes an API-key record after the caller has decided
// that no audit tombstone is required. This is used by privileged, short-lived
// production checks so daily smoke tests cannot grow api-keys.json forever.
// Normal admin DELETE requests still revoke and retain their audit record.
func (s *apiKeyStore) remove(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Keys {
		if s.Keys[i].ID != id {
			continue
		}
		next := make([]apiKeyRecord, 0, len(s.Keys)-1)
		next = append(next, s.Keys[:i]...)
		next = append(next, s.Keys[i+1:]...)
		if err := s.saveKeysLocked(next); err != nil {
			return false, err
		}
		s.Keys = next
		return true, nil
	}
	return false, nil
}
func (s *apiKeyStore) valid(raw string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := keyHash(raw)
	now := time.Now()
	for i := range s.Keys {
		if subtle.ConstantTimeCompare([]byte(s.Keys[i].Hash), []byte(h)) == 1 && !s.Keys[i].Revoked {
			if s.Keys[i].ExpiresAt != nil && now.After(*s.Keys[i].ExpiresAt) {
				// expired key: not valid, but keep the record visible in the UI
				continue
			}
			s.Keys[i].LastUsedAt = &now
			return true
		}
	}
	return false
}
func (s *apiKeyStore) setExpiry(id string, days int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Keys {
		if s.Keys[i].ID == id {
			next := append([]apiKeyRecord(nil), s.Keys...)
			if days > 0 {
				exp := time.Now().AddDate(0, 0, days)
				next[i].ExpiresAt = &exp
			} else {
				next[i].ExpiresAt = nil
			}
			if err := s.saveKeysLocked(next); err != nil {
				return false, err
			}
			s.Keys = next
			return true, nil
		}
	}
	return false, nil
}
