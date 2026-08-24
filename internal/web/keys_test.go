package web

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAPIKeyMutationCommitsOnlyAfterPersistence(t *testing.T) {
	dir := t.TempDir()
	s := &apiKeyStore{path: filepath.Join(dir, "keys.json")}
	rec, raw, err := s.create("test", 0)
	if err != nil || !s.valid(raw) {
		t.Fatalf("create/valid failed: %v", err)
	}
	s.path = filepath.Join(dir, "missing", "child", string([]byte{0}), "keys.json")
	ok, err := s.revoke(rec.ID)
	if err == nil || ok {
		t.Fatalf("expected persistence failure, ok=%v err=%v", ok, err)
	}
	if s.Keys[0].Revoked {
		t.Fatal("failed persistence mutated in-memory key")
	}
}

func TestAPIKeyPathCannotBeOverriddenByJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(path, []byte(`{"Path":"/tmp/attacker","keys":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_API_KEYS", path)
	s, err := openAPIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if s.path != path {
		t.Fatalf("path overridden: %q", s.path)
	}
}

func TestAPIKeyRemovePersistsWithoutAffectingOtherKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	s := &apiKeyStore{path: path}
	removed, removedRaw, err := s.create("temporary-check", 1)
	if err != nil {
		t.Fatal(err)
	}
	kept, keptRaw, err := s.create("permanent", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.remove(removed.ID); err != nil || !ok {
		t.Fatalf("remove failed: ok=%v err=%v", ok, err)
	}
	if s.valid(removedRaw) {
		t.Fatal("removed key remained valid")
	}
	if !s.valid(keptRaw) {
		t.Fatal("unrelated key became invalid")
	}
	t.Setenv("M365_API_KEYS", path)
	reloaded, err := openAPIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Keys) != 1 || reloaded.Keys[0].ID != kept.ID {
		t.Fatalf("unexpected persisted keys: %+v", reloaded.Keys)
	}
}

func TestAPIKeyRemoveCommitsOnlyAfterPersistence(t *testing.T) {
	dir := t.TempDir()
	s := &apiKeyStore{path: filepath.Join(dir, "keys.json")}
	rec, raw, err := s.create("temporary-check", 1)
	if err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(dir, "missing", "child", string([]byte{0}), "keys.json")
	if ok, err := s.remove(rec.ID); err == nil || ok {
		t.Fatalf("expected persistence failure, ok=%v err=%v", ok, err)
	}
	if len(s.Keys) != 1 || !s.valid(raw) {
		t.Fatal("failed persistence mutated in-memory keys")
	}
}
