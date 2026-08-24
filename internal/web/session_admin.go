package web

import (
	"encoding/json"
	"net/http"
	"time"
)

type sessionCacheStats struct {
	TotalRecords    int        `json:"totalRecords"`
	StableRecords   int        `json:"stableRecords"`
	ResponseAliases int        `json:"responseAliases"`
	PinnedRecords   int        `json:"pinnedRecords"`
	SerializedBytes int64      `json:"serializedBytes"`
	OldestUpdatedAt *time.Time `json:"oldestUpdatedAt,omitempty"`
	LatestUpdatedAt *time.Time `json:"latestUpdatedAt,omitempty"`
}

func (s *sessionStore) cacheStatsLocked() sessionCacheStats {
	stats := sessionCacheStats{TotalRecords: len(s.data)}
	for id, value := range s.data {
		if value.ResponseAlias {
			stats.ResponseAliases++
		} else {
			stats.StableRecords++
		}
		if s.pins[id] > 0 {
			stats.PinnedRecords++
		}
		if value.UpdatedAt.IsZero() {
			continue
		}
		updated := value.UpdatedAt
		if stats.OldestUpdatedAt == nil || updated.Before(*stats.OldestUpdatedAt) {
			copy := updated
			stats.OldestUpdatedAt = &copy
		}
		if stats.LatestUpdatedAt == nil || updated.After(*stats.LatestUpdatedAt) {
			copy := updated
			stats.LatestUpdatedAt = &copy
		}
	}
	encoded, _ := json.MarshalIndent(s.data, "", "  ")
	stats.SerializedBytes = int64(len(encoded))
	return stats
}

func (s *sessionStore) cacheStats() sessionCacheStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cacheStatsLocked()
}

func (s *sessionStore) pruneNow(now time.Time) (sessionCacheStats, sessionCacheStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	before := s.cacheStatsLocked()
	previous := cloneConversations(s.data)
	encoded := s.pruneLocked(now.UTC())
	if err := atomicWriteFile(s.path, encoded, 0o600); err != nil {
		s.data = previous
		return before, s.cacheStatsLocked(), err
	}
	after := s.cacheStatsLocked()
	after.SerializedBytes = int64(len(encoded))
	return before, after, nil
}

func (s *Server) adminSessionCache(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, map[string]any{"stats": s.sessions.cacheStats()})
	case http.MethodPost:
		var body struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Action != "prune" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "action must be prune")
			return
		}
		before, after, err := s.sessions.pruneNow(time.Now())
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "storage_error", "could not persist the pruned session cache")
			return
		}
		jsonOut(w, map[string]any{"status": "pruned", "before": before, "after": after})
	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}
