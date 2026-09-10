package weed_server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
// CSD-capable replicas first, then least in-flight bytes, then fewest in-flight
// requests, then lower probe latency, then lexicographic host as a deterministic
// tie-breaker.
//
// Capability probing happens outside csdSchedMu (see probeReplicaCapabilities);
// only the in-memory load snapshot is read under the lock, so a slow
// /compute/status probe can no longer serialize concurrent chunk fan-out.
func (fs *FilerServer) rankComputeReplicas(ctx context.Context, urlStrings []string) []string {
	candidates := fs.probeReplicaCapabilities(ctx, urlStrings)

	fs.csdSchedMu.Lock()
	ranked := rankCandidates(candidates, fs.csdLoadOfLocked, nil)
	fs.csdSchedMu.Unlock()

	return csdCandidateURLs(ranked)
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

// rankAndReserve picks the current best replica that is not in exclude, and
// atomically reserves its in-flight accounting so concurrent fan-out requests
// spread across the best CSD replicas instead of stampeding onto the first one.
//
// Capability probing (which may hit the network) happens before csdSchedMu is
// taken; the critical section only ranks an in-memory snapshot and updates the
// counters.
//
// weightBytes is the payload size of the chunk being dispatched. In-flight load
// is tracked as bytes rather than request counts so the scheduler reflects real
// device and link occupancy instead of treating a 64KiB chunk like an 8MiB one.
//
// The returned release function must be called when the attempt finishes, on
// both the success and the failure path.
func (fs *FilerServer) rankAndReserve(ctx context.Context, urlStrings []string, weightBytes int64, exclude map[string]struct{}) (string, func(), error) {
	candidates := fs.probeReplicaCapabilities(ctx, urlStrings)
	if weightBytes <= 0 {
		weightBytes = 1
	}

	fs.csdSchedMu.Lock()
	if fs.csdInflightBytes == nil {
		fs.csdInflightBytes = make(map[string]int64)
	}
	if fs.csdInflightReqs == nil {
		fs.csdInflightReqs = make(map[string]int64)
	}
	ranked := rankCandidates(candidates, fs.csdLoadOfLocked, exclude)
	if len(ranked) == 0 {
		fs.csdSchedMu.Unlock()
		if len(exclude) > 0 {
			return "", func() {}, fmt.Errorf("no compute replica left after excluding %d failed replica(s)", len(exclude))
		}
		return "", func() {}, fmt.Errorf("no rankable compute replica")
	}
	chosen := ranked[0]
	fs.csdInflightBytes[chosen.Host] += weightBytes
	fs.csdInflightReqs[chosen.Host]++
	fs.csdSchedMu.Unlock()

	var released bool
	return chosen.URL, func() {
		fs.csdSchedMu.Lock()
		defer fs.csdSchedMu.Unlock()
		if released {
			return
		}
		released = true
		fs.csdInflightBytes[chosen.Host] -= weightBytes
		if fs.csdInflightBytes[chosen.Host] < 0 {
			fs.csdInflightBytes[chosen.Host] = 0
		}
		fs.csdInflightReqs[chosen.Host]--
		if fs.csdInflightReqs[chosen.Host] < 0 {
			fs.csdInflightReqs[chosen.Host] = 0
		}
	}, nil
}
