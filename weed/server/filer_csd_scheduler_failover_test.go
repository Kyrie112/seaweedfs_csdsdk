package weed_server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func startCSDStatusServer(t *testing.T, enabled bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != csdStatusPath {
			http.NotFound(w, r)
			return
		}
		if enabled {
			w.Write([]byte(`{"csd_enabled":true,"csd_endpoint":"http://127.0.0.1:18090"}`))
			return
		}
		w.Write([]byte(`{"csd_enabled":false}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newSchedulerTestFiler() *FilerServer {
	return &FilerServer{
		csdCache:         make(map[string]csdReplicaCapability),
		csdInflightBytes: make(map[string]int64),
		csdInflightReqs:  make(map[string]int64),
	}
}

// TestRankAndReserveSkipsExcludedReplicas is the regression test for failover:
// once a replica has failed, the next attempt must not be routed back to it.
func TestRankAndReserveSkipsExcludedReplicas(t *testing.T) {
	first := startCSDStatusServer(t, true)
	second := startCSDStatusServer(t, true)
	third := startCSDStatusServer(t, true)

	fs := newSchedulerTestFiler()
	urls := []string{first.URL, second.URL, third.URL}

	excluded := map[string]struct{}{
		csdHostOf(first.URL):  {},
		csdHostOf(second.URL): {},
	}
	chosen, release, err := fs.rankAndReserve(context.Background(), urls, 4096, excluded)
	if err != nil {
		t.Fatalf("rankAndReserve with exclusions: %v", err)
	}
	defer release()
	if chosen != third.URL {
		t.Fatalf("rankAndReserve chose %q, want the only non-excluded replica %q", chosen, third.URL)
	}
}

func TestRankAndReserveFailsWhenAllReplicasExcluded(t *testing.T) {
	only := startCSDStatusServer(t, true)
	fs := newSchedulerTestFiler()

	excluded := map[string]struct{}{csdHostOf(only.URL): {}}
	if _, _, err := fs.rankAndReserve(context.Background(), []string{only.URL}, 4096, excluded); err == nil {
		t.Fatal("expected an error when every replica has been excluded, got nil")
	}
}

// TestRankComputeReplicasPrefersLowerByteLoad pins the primary soft constraint:
// few large chunks already in flight must outrank many small ones, because the
// scheduler models device and link occupancy in bytes.
func TestRankComputeReplicasPrefersLowerByteLoad(t *testing.T) {
	busyWithOneLargeChunk := startCSDStatusServer(t, true)
	busyWithManySmallChunks := startCSDStatusServer(t, true)

	fs := newSchedulerTestFiler()
	fs.csdInflightBytes[csdHostOf(busyWithOneLargeChunk.URL)] = 8 << 20
	fs.csdInflightReqs[csdHostOf(busyWithOneLargeChunk.URL)] = 1
	fs.csdInflightBytes[csdHostOf(busyWithManySmallChunks.URL)] = 10 * (64 << 10)
	fs.csdInflightReqs[csdHostOf(busyWithManySmallChunks.URL)] = 10

	ranked := fs.rankComputeReplicas(context.Background(), []string{busyWithOneLargeChunk.URL, busyWithManySmallChunks.URL})
	if ranked[0] != busyWithManySmallChunks.URL {
		t.Fatalf("byte-weighted scheduler picked the replica holding %d bytes first, want the one holding %d",
			8<<20, 10*(64<<10))
	}
}

// TestRankComputeReplicasUsesRequestCountAsTieBreaker covers the secondary
// signal: equal byte load, fewer requests wins.
func TestRankComputeReplicasUsesRequestCountAsTieBreaker(t *testing.T) {
	manyRequests := startCSDStatusServer(t, true)
	fewRequests := startCSDStatusServer(t, true)

	fs := newSchedulerTestFiler()
	fs.csdInflightBytes[csdHostOf(manyRequests.URL)] = 4096
	fs.csdInflightReqs[csdHostOf(manyRequests.URL)] = 4
	fs.csdInflightBytes[csdHostOf(fewRequests.URL)] = 4096
	fs.csdInflightReqs[csdHostOf(fewRequests.URL)] = 1

	ranked := fs.rankComputeReplicas(context.Background(), []string{manyRequests.URL, fewRequests.URL})
	if ranked[0] != fewRequests.URL {
		t.Fatal("request count tie-breaker did not prefer the replica with fewer in-flight requests")
	}
}

func TestRankAndReserveReleasesByteLoad(t *testing.T) {
	only := startCSDStatusServer(t, true)
	fs := newSchedulerTestFiler()
	const weight = int64(8 << 20)

	_, release, err := fs.rankAndReserve(context.Background(), []string{only.URL}, weight, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fs.csdInflightBytes[csdHostOf(only.URL)]; got != weight {
		t.Fatalf("reserved bytes = %d, want %d", got, weight)
	}
	release()
	if got := fs.csdInflightBytes[csdHostOf(only.URL)]; got != 0 {
		t.Fatalf("bytes after release = %d, want 0", got)
	}
	if got := fs.csdInflightReqs[csdHostOf(only.URL)]; got != 0 {
		t.Fatalf("requests after release = %d, want 0", got)
	}
	// Releasing twice must not corrupt the counters.
	release()
	if got := fs.csdInflightBytes[csdHostOf(only.URL)]; got != 0 {
		t.Fatalf("bytes after double release = %d, want 0", got)
	}
}

// TestRankAndReserveDoesNotSerializeCapabilityProbes is the regression test for
// the lock-granularity bug: capability probing performs network I/O and must not
// happen while csdSchedMu is held, otherwise concurrent chunk fan-out is
// serialized behind a slow /compute/status probe.
//
// The status handler blocks until both ranking calls have entered it. With the
// bug present the second call can never enter (it is stuck on the mutex) and the
// test fails on timeout instead of hanging.
func TestRankAndReserveDoesNotSerializeCapabilityProbes(t *testing.T) {
	const callers = 2
	entered := make(chan struct{}, callers)
	probeGate := make(chan struct{})
	var closeGateOnce sync.Once
	closeGate := func() { closeGateOnce.Do(func() { close(probeGate) }) }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-probeGate
		w.Write([]byte(`{"csd_enabled":true}`))
	}))
	t.Cleanup(func() {
		closeGate()
		srv.Close()
	})

	fs := newSchedulerTestFiler()
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := fs.rankAndReserve(context.Background(), []string{srv.URL}, 1024, nil)
			if err != nil {
				t.Errorf("rankAndReserve: %v", err)
				return
			}
			release()
		}()
	}

	for i := 0; i < callers; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("capability probes were serialized: a second rankAndReserve could not reach /compute/status")
		}
	}
	closeGate()
	wg.Wait()
}
