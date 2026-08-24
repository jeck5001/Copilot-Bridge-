package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdminSessionCacheStatsDoNotExposeHistory(t *testing.T) {
	now := time.Now().UTC()
	store := &sessionStore{
		path: filepath.Join(t.TempDir(), "sessions.json"),
		data: map[string]conversation{
			"stable": {ID: "stable", UpdatedAt: now, PortableMessages: []oaiMsg{{Role: "user", Content: json.RawMessage(`"top-secret"`)}}},
			"resp_1": {ID: "resp_1", AliasGroup: "stable", ResponseAlias: true, UpdatedAt: now},
		},
		pins: map[string]int{"stable": 1},
	}
	s := &Server{sessions: store}
	w := httptest.NewRecorder()
	s.adminSessionCache(w, httptest.NewRequest(http.MethodGet, "/api/admin/session-cache", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET=%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "top-secret") || strings.Contains(w.Body.String(), "stable\"") {
		t.Fatalf("session content or identifiers leaked: %s", w.Body.String())
	}
	var response struct {
		Stats sessionCacheStats `json:"stats"`
	}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Stats.TotalRecords != 2 || response.Stats.StableRecords != 1 || response.Stats.ResponseAliases != 1 || response.Stats.PinnedRecords != 1 {
		t.Fatalf("unexpected stats: %+v", response.Stats)
	}
}

func TestAdminSessionCachePrunePersistsAndPreservesPinned(t *testing.T) {
	now := time.Now().UTC()
	store := &sessionStore{
		path: filepath.Join(t.TempDir(), "sessions.json"),
		data: map[string]conversation{
			"expired":        {ID: "expired", UpdatedAt: now.Add(-sessionTTL - time.Hour)},
			"pinned-expired": {ID: "pinned-expired", UpdatedAt: now.Add(-sessionTTL - time.Hour)},
			"current":        {ID: "current", UpdatedAt: now},
		},
		pins: map[string]int{"pinned-expired": 1},
	}
	s := &Server{sessions: store}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/admin/session-cache", strings.NewReader(`{"action":"prune"}`))
	s.adminSessionCache(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST=%d %s", w.Code, w.Body.String())
	}
	if _, ok := store.data["expired"]; ok {
		t.Fatal("expired unpinned record was not pruned")
	}
	if _, ok := store.data["pinned-expired"]; !ok {
		t.Fatal("pinned record was pruned")
	}
	if _, err := openSessionStoreAtForTest(store.path); err != nil {
		t.Fatalf("persisted cache is invalid: %v", err)
	}
}

func openSessionStoreAtForTest(path string) (*sessionStore, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data map[string]conversation
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	return &sessionStore{path: path, data: data, pins: map[string]int{}}, nil
}
