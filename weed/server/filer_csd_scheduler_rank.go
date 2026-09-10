package weed_server

import (
	"context"
	"net/url"
	"sort"
	"sync"
)

// csdCandidate couples a replica URL with the capability facts the scheduler
// needs. Capability probing happens before any scheduling lock is taken, so a
// slow /compute/status probe can never serialize concurrent chunk fan-out.
type csdCandidate struct {
	URL            string
	Host           string
	CSDEnabled     bool
	ProbeLatencyMs int64
}

// csdLoad is the filer-side view of how much work is already in flight on a
// replica. Bytes are tracked separately from request counts because a single
// 8MiB chunk occupies far more device and link bandwidth than a 64KiB chunk,
// while a long tail of small requests still costs per-request device overhead.
type csdLoad struct {
	Bytes int64
	Reqs  int64
}

// probeReplicaCapabilities resolves (and caches) the CSD capability of every
// replica in urlStrings. Cache misses perform a network probe, so the caller
// must not hold csdSchedMu while calling this.
func (fs *FilerServer) probeReplicaCapabilities(ctx context.Context, urlStrings []string) []csdCandidate {
	candidates := make([]csdCandidate, len(urlStrings))
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
				candidates[idx] = csdCandidate{URL: raw, Host: raw}
				return
			}
			capability := fs.computeReplicaCapability(ctx, u)
			candidates[idx] = csdCandidate{
				URL:            raw,
				Host:           capability.Host,
				CSDEnabled:     capability.CSDEnabled,
				ProbeLatencyMs: capability.ProbeLatencyMs,
			}
		}(i, raw)
	}
	wg.Wait()
	return candidates
}

// rankCandidates orders candidates by the scheduling objective:
//
//  1. hard constraint - CSD capability;
//  2. least in-flight bytes (bandwidth-aware load);
//  3. fewest in-flight requests (per-request device overhead);
//  4. lower status-probe latency;
//  5. deterministic host tie-breaker.
//
// Replicas listed in exclude are dropped. That is what makes the failover loop
// in fetchChunkComputeResult meaningful: every retry is guaranteed to target a
// replica that has not failed yet, instead of re-ranking the same broken one
// back to the front.
func rankCandidates(candidates []csdCandidate, loadOf func(host string) csdLoad, exclude map[string]struct{}) []csdCandidate {
	ranked := make([]csdCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if exclude != nil {
			if _, skipped := exclude[candidate.Host]; skipped {
				continue
			}
		}
		ranked = append(ranked, candidate)
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.CSDEnabled != b.CSDEnabled {
			return a.CSDEnabled
		}
		var loadA, loadB csdLoad
		if loadOf != nil {
			loadA, loadB = loadOf(a.Host), loadOf(b.Host)
		}
		if loadA.Bytes != loadB.Bytes {
			return loadA.Bytes < loadB.Bytes
		}
		if loadA.Reqs != loadB.Reqs {
			return loadA.Reqs < loadB.Reqs
		}
		if a.ProbeLatencyMs != b.ProbeLatencyMs {
			return a.ProbeLatencyMs < b.ProbeLatencyMs
		}
		return a.Host < b.Host
	})
	return ranked
}

// csdCandidateURLs projects ranked candidates back to the URL list callers use
// for HTTP dispatch.
func csdCandidateURLs(ranked []csdCandidate) []string {
	urls := make([]string, 0, len(ranked))
	for _, candidate := range ranked {
		urls = append(urls, candidate.URL)
	}
	return urls
}

// csdHostOf extracts the host key used by the in-flight counters. Requests are
// dispatched against whatever URL the master returned, so the host - not the
// full URL - is the unit of load accounting.
func csdHostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Host
}

// csdLoadOfLocked must be called with csdSchedMu held.
func (fs *FilerServer) csdLoadOfLocked(host string) csdLoad {
	return csdLoad{
		Bytes: fs.csdInflightBytes[host],
		Reqs:  fs.csdInflightReqs[host],
	}
}
