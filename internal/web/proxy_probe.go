package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vipamess/Copilot-Bridge-/internal/proxy"
)

const (
	egressProbeURL              = "https://api.ipify.org?format=json"
	egressProbeTimeout          = 15 * time.Second
	egressProbeMaxBody    int64 = 64 << 10
	maxEgressProbeWorkers       = 4
)

type egressProbeResult struct {
	OK           bool   `json:"ok"`
	Direct       bool   `json:"direct,omitempty"`
	IP           string `json:"ip,omitempty"`
	Status       int    `json:"status,omitempty"`
	LatencyMs    int64  `json:"latencyMs"`
	EgressHTTPMs int64  `json:"egressHttpMs"`
	Error        string `json:"error,omitempty"`
}

var egressProbeRunner = probeEgress

func canonicalProxyIdentity(cfg proxy.Config) string {
	if cfg.Type == proxy.KindDirect {
		return "direct"
	}
	return fmt.Sprintf("%s|%t|%s|%s|%s|%s", cfg.Type, cfg.UseTLS,
		strings.ToLower(cfg.Host), cfg.Port, cfg.User, cfg.Pass)
}

// probeEgress performs one bounded, cold HTTP request through the configured
// egress. Its duration is deliberately end-to-end: it stops only after a
// size-limited response body has been read. It is not the RTT between servers.
func probeEgress(parent context.Context, rawProxy, targetURL string) egressProbeResult {
	cfg, err := proxy.Parse(strings.TrimSpace(rawProxy))
	if err != nil {
		return egressProbeResult{Error: "invalid proxy configuration"}
	}
	ctx, cancel := context.WithTimeout(parent, egressProbeTimeout)
	defer cancel()

	client, err := cfg.HTTPClient()
	if err != nil {
		return egressProbeResult{Direct: cfg.Type == proxy.KindDirect, Error: "egress client unavailable"}
	}
	if cfg.Type == proxy.KindDirect {
		// http.DefaultClient may reuse an unrelated warm connection, which made
		// direct and proxy rows incomparable. Use a fresh transport for every
		// probe, matching the cold connection semantics of proxy clients.
		client = &http.Client{Transport: &http.Transport{}, Timeout: egressProbeTimeout}
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return egressProbeResult{Direct: cfg.Type == proxy.KindDirect, Error: "invalid egress probe request"}
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		elapsed := time.Since(start).Milliseconds()
		return egressProbeResult{
			Direct: cfg.Type == proxy.KindDirect, LatencyMs: elapsed,
			EgressHTTPMs: elapsed, Error: "egress connection failed",
		}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, egressProbeMaxBody+1))
	elapsed := time.Since(start).Milliseconds()
	result := egressProbeResult{
		Direct: cfg.Type == proxy.KindDirect, Status: resp.StatusCode,
		LatencyMs: elapsed, EgressHTTPMs: elapsed,
	}
	if readErr != nil {
		result.Error = "egress response read failed"
		return result
	}
	if int64(len(body)) > egressProbeMaxBody {
		result.Error = "egress response exceeded size limit"
		return result
	}
	var payload struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		result.Error = "egress response was not valid JSON"
		return result
	}
	result.IP = strings.TrimSpace(payload.IP)
	result.OK = resp.StatusCode == http.StatusOK && result.IP != ""
	if !result.OK {
		result.Error = fmt.Sprintf("egress probe returned HTTP %d", resp.StatusCode)
	}
	return result
}
