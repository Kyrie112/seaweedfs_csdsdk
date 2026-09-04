package weed_server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRankComputeReplicasPrefersCSD(t *testing.T) {
	csdServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != csdStatusPath {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"csd_enabled":true,"csd_endpoint":"http://127.0.0.1:18090"}`))
	}))
	defer csdServer.Close()

	cpuServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != csdStatusPath {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"csd_enabled":false}`))
	}))
	defer cpuServer.Close()

	fs := &FilerServer{csdCache: make(map[string]csdReplicaCapability)}
	urls := []string{csdServer.URL, cpuServer.URL}
	for i := 0; i < 100; i++ {
		ranked := fs.rankComputeReplicas(context.Background(), urls)
		if len(ranked) != 2 {
			t.Fatalf("iteration %d: got %d ranked replicas, want 2", i, len(ranked))
		}
		if ranked[0] != csdServer.URL {
			t.Fatalf("iteration %d: CSD-aware scheduler picked %q instead of CSD replica %q",
				i, ranked[0], csdServer.URL)
		}
	}
}

func TestRankComputeReplicasStableWhenAllCSD(t *testing.T) {
	var servers []*httptest.Server
	var urls []string
	for i := 0; i < 3; i++ {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"csd_enabled":true}`))
		}))
		defer srv.Close()
		servers = append(servers, srv)
		urls = append(urls, srv.URL)
	}
	fs := &FilerServer{csdCache: make(map[string]csdReplicaCapability)}
	first := fs.rankComputeReplicas(context.Background(), urls)[0]
	for i := 0; i < 50; i++ {
		if got := fs.rankComputeReplicas(context.Background(), urls)[0]; got != first {
			t.Fatalf("iteration %d: ranking changed across runs: %q vs %q", i, got, first)
		}
	}
}

func TestRankComputeReplicasPrefersLessLoaded(t *testing.T) {
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"csd_enabled":true}`))
	}))
	defer busy.Close()
	idle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"csd_enabled":true}`))
	}))
	defer idle.Close()

	fs := &FilerServer{
		csdCache:    make(map[string]csdReplicaCapability),
		csdInflight: map[string]int64{busy.URL: 5},
	}
	ranked := fs.rankComputeReplicas(context.Background(), []string{busy.URL, idle.URL})
	if ranked[0] != idle.URL {
		t.Fatalf("load-aware scheduler picked busy %q, want idle %q", ranked[0], idle.URL)
	}
}

func TestRankAndReserveIncrementsInflight(t *testing.T) {
	one := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"csd_enabled":true}`))
	}))
	defer one.Close()
	two := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"csd_enabled":true}`))
	}))
	defer two.Close()

	fs := &FilerServer{
		csdCache:    make(map[string]csdReplicaCapability),
		csdInflight: make(map[string]int64),
	}
	first, releaseFirst, err := fs.rankAndReserve(context.Background(), []string{one.URL, two.URL})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := fs.rankAndReserve(context.Background(), []string{one.URL, two.URL})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("rankAndReserve selected the same replica twice: %s", first)
	}
	releaseFirst()
	third, _, err := fs.rankAndReserve(context.Background(), []string{one.URL, two.URL})
	if err != nil {
		t.Fatal(err)
	}
	if third != first {
		t.Fatalf("after release, expected original first replica %s, got %s", first, third)
	}
}
