package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vipamess/Copilot-Bridge-/internal/proxy"
)

func TestProbeEgressDirectMeasuresCompleteBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ip":"203.0.113.9"`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(35 * time.Millisecond)
		_, _ = w.Write([]byte(`}`))
	}))
	defer upstream.Close()

	result := probeEgress(context.Background(), "", upstream.URL)
	if !result.OK || !result.Direct || result.IP != "203.0.113.9" {
		t.Fatalf("unexpected direct probe: %+v", result)
	}
	if result.EgressHTTPMs < 30 || result.LatencyMs != result.EgressHTTPMs {
		t.Fatalf("probe stopped before complete body: %+v", result)
	}
}

func TestCanonicalProxyIdentityNormalizesEquivalentSOCKSURLs(t *testing.T) {
	left, err := proxyConfigForTest("socks5://EXAMPLE.test:7915")
	if err != nil {
		t.Fatal(err)
	}
	right, err := proxyConfigForTest("socks5h://example.TEST:7915")
	if err != nil {
		t.Fatal(err)
	}
	if canonicalProxyIdentity(left) != canonicalProxyIdentity(right) {
		t.Fatalf("equivalent proxies did not normalize: %q != %q", canonicalProxyIdentity(left), canonicalProxyIdentity(right))
	}
}

func proxyConfigForTest(raw string) (proxy.Config, error) {
	return proxy.Parse(raw)
}

func TestAllProxyProbeDeduplicatesLimitsConcurrencyAndPreservesOrder(t *testing.T) {
	ids := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		ids = append(ids, fmt.Sprintf("account-%02d", i+1))
	}
	server := newStickyAccountTestServer(t, ids...)
	for i, id := range ids {
		port := 10000 + i
		raw := fmt.Sprintf("socks5://127.0.0.1:%d", port)
		if i == 1 {
			raw = "socks5h://127.0.0.1:10000"
		}
		if err := server.tokens.SetProxy(id, raw); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := json.Marshal(server.tokens.List())

	originalRunner := egressProbeRunner
	defer func() { egressProbeRunner = originalRunner }()
	var mu sync.Mutex
	active, maxActive, calls := 0, 0, 0
	egressProbeRunner = func(_ context.Context, _ string, _ string) egressProbeResult {
		mu.Lock()
		active++
		calls++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		time.Sleep(15 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return egressProbeResult{OK: true, IP: "203.0.113.10", Status: 200, LatencyMs: 15, EgressHTTPMs: 15}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/test-all-proxies", bytes.NewReader(nil))
	recorder := httptest.NewRecorder()
	server.testAllProxies(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Results []struct {
			AccountID    string `json:"accountId"`
			EgressHTTPMs int64  `json:"egressHttpMs"`
		} `json:"results"`
		UniqueEgress int `json:"uniqueEgress"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if calls != 9 || response.UniqueEgress != 9 {
		t.Fatalf("equivalent proxy was not deduplicated: calls=%d unique=%d", calls, response.UniqueEgress)
	}
	if maxActive > maxEgressProbeWorkers {
		t.Fatalf("probe concurrency=%d exceeds limit=%d", maxActive, maxEgressProbeWorkers)
	}
	if len(response.Results) != len(ids) {
		t.Fatalf("result count=%d want=%d", len(response.Results), len(ids))
	}
	for i, result := range response.Results {
		if result.AccountID != ids[i] || result.EgressHTTPMs != 15 {
			t.Fatalf("result %d=%+v want account=%s", i, result, ids[i])
		}
	}
	after, _ := json.Marshal(server.tokens.List())
	if !bytes.Equal(before, after) {
		t.Fatal("read-only egress probe mutated account configuration")
	}
}
