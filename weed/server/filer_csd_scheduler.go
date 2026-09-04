package weed_server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

const (
	csdStatusPath       = "/compute/status"
	csdCacheTTL         = 30 * time.Second
	csdProbeTimeout     = 750 * time.Millisecond
	csdProbeConcurrency = 8
)

type csdReplicaCapability struct {
	Host           string
	BaseURL        string
	CSDEnabled     bool
	CSDEndpoint    string
	ProbeLatencyMs int64
	ProbedAt       time.Time
	ProbeErr       error
}

// rankComputeReplicas returns volume URLs ordered for compute execution:
// CSD-capable replicas first, then lower probe latency, then lexicographic
// host as a deterministic tie-breaker.
func (fs *FilerServer) rankComputeReplicas(ctx context.Context, urlStrings []string) []string {
	type capResult struct {
		index int
		cap   csdReplicaCapability
	}
	results := make([]capResult, len(urlStrings))
	sem := make(chan struct{}, csdProbeConcurrency)
	var wg sync.WaitGroup
	for i, raw := range urlStrings {
		wg.Add(1)
		go func(idx int, raw string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			u, err := url.Parse(raw)
			if err != nil {
				results[idx] = capResult{idx, csdReplicaCapability{BaseURL: raw, ProbeErr: err}}
				return
			}
			results[idx] = capResult{idx, fs.computeReplicaCapability(ctx, u)}
		}(i, raw)
	}
	wg.Wait()

	sort.SliceStable(results, func(i, j int) bool {
		a, b := results[i].cap, results[j].cap
		if a.CSDEnabled != b.CSDEnabled {
			return a.CSDEnabled
		}
		if a.ProbeLatencyMs != b.ProbeLatencyMs {
			return a.ProbeLatencyMs < b.ProbeLatencyMs
		}
		return a.Host < b.Host
	})
	ranked := make([]string, 0, len(results))
	for _, r := range results {
		ranked = append(ranked, urlStrings[r.index])
	}
	return ranked
}

func (fs *FilerServer) computeReplicaCapability(ctx context.Context, base *url.URL) csdReplicaCapability {
	host := base.Host
	fs.csdMu.RLock()
	cached, ok := fs.csdCache[host]
	fs.csdMu.RUnlock()
	if ok && time.Since(cached.ProbedAt) < csdCacheTTL {
		return cached
	}

	statusURL := *base
	statusURL.Path = csdStatusPath
	statusURL.RawQuery = ""
	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, csdProbeTimeout)
	defer cancel()

	var resp csdStatusResponse
	err := fs.probeCSDStatus(probeCtx, statusURL.String(), &resp)
	cap := csdReplicaCapability{
		Host:           host,
		BaseURL:        base.String(),
		CSDEnabled:     err == nil && resp.CSDEnabled,
		CSDEndpoint:    resp.CSDEndpoint,
		ProbeLatencyMs: time.Since(start).Milliseconds(),
		ProbedAt:       time.Now(),
		ProbeErr:       err,
	}
	fs.csdMu.Lock()
	fs.csdCache[host] = cap
	fs.csdMu.Unlock()
	if err != nil {
		glog.V(4).Infof("CSD status probe %s: %v", host, err)
	}
	return cap
}

func (fs *FilerServer) probeCSDStatus(ctx context.Context, statusURL string, out *csdStatusResponse) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: csdProbeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status code %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return err
	}
	return nil
}

// pickComputeReplica returns a ranked candidate for one chunk. Multiple chunks
// independently pick their best replica, which balances CSD-capable volume
// servers across the fan-out.
func pickComputeReplica(_ []string, ranked []string) string {
	for _, u := range ranked {
		if strings.TrimSpace(u) != "" {
			return u
		}
	}
	return ""
}
