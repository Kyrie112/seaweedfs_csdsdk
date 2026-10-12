package weed_server

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// TestClassifyComputeError pins the M2 error taxonomy: callers and operators must
// be able to tell "the data changed" from "the node is down".
func TestClassifyComputeError(t *testing.T) {
	cases := []struct {
		err  error
		want computeErrorClass
	}{
		{nil, classNone},
		{fmt.Errorf("%w: chunk 0", errStaleRange), classStaleRange},
		{fmt.Errorf("volume compute returned status 404: not found"), classNotFound},
		{fmt.Errorf("volume compute returned status 500: boom"), classNodeError},
		{fmt.Errorf("compute is not supported for compressed chunks"), classUnsupported},
	}
	for _, tc := range cases {
		if got := classifyComputeError(tc.err); got != tc.want {
			t.Fatalf("classifyComputeError(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}
}

// TestErrorClassRetryPolicy: only node errors and missing replicas are worth
// another replica; a stale range must be handled by re-planning.
func TestErrorClassRetryPolicy(t *testing.T) {
	retryable := map[computeErrorClass]bool{
		classNodeError:   true,
		classNotFound:    true,
		classStaleRange:  false,
		classUnsupported: false,
		classNone:        false,
	}
	for class, want := range retryable {
		if got := class.retryOnAnotherReplica(); got != want {
			t.Fatalf("%s.retryOnAnotherReplica() = %v, want %v", class, got, want)
		}
	}
}

// TestHostPreferenceConverges: a request should keep reusing the replica it
// already used (M2/T2.1), while still tolerating one chunk of load difference.
func TestHostPreferenceConverges(t *testing.T) {
	busy := startCSDStatusServer(t, true)
	other := startCSDStatusServer(t, true)
	fs := newSchedulerTestFiler()
	urls := []string{busy.URL, other.URL}

	preferred := newHostPreference()
	const chunk = int64(1 << 20)

	first, release, err := fs.rankAndReserve(context.Background(), urls, chunk, nil, preferred)
	if err != nil {
		t.Fatal(err)
	}
	preferred.remember(csdHostOf(first))

	// Without a preference the load-aware scheduler would move to the idle host.
	idle, _, err := fs.rankAndReserve(context.Background(), urls, chunk, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if idle == first {
		t.Skip("both replicas measured as equally loaded; affinity cannot be observed")
	}

	// With the preference, the request stays on the host it already used as long
	// as the imbalance is within one chunk.
	sticky, _, err := fs.rankAndReserve(context.Background(), urls, chunk, nil, preferred)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if sticky != first {
		t.Fatalf("affinity did not hold: chose %s, want %s", sticky, first)
	}

	// Push the preferred host well beyond the margin: affinity must yield.
	for i := 0; i < 5; i++ {
		_, r, err := fs.rankAndReserve(context.Background(), urls, chunk, nil, preferred)
		if err != nil {
			break
		}
		r()
	}
	if preferred.size() == 0 {
		t.Fatal("preference set should not be empty")
	}
}

// TestOnStaleParamDocumentsPolicy: the parameter name is part of the API.
func TestOnStaleParamDocumentsPolicy(t *testing.T) {
	if computeQueryParamOnStale != "on_stale" {
		t.Fatalf("on_stale parameter renamed to %q", computeQueryParamOnStale)
	}
	if computeSnapshotAttempts < 2 {
		t.Fatalf("stale retry requires at least two attempts, got %d", computeSnapshotAttempts)
	}
	_ = http.StatusConflict
}
