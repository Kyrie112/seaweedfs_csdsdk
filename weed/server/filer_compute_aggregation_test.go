package weed_server

import (
	"math/big"
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// TestAggregateAdditiveResultsWrapsToUint64 pins the cross-chunk semantics: the
// aggregate of a packing operator is the sum of the per-chunk 64-bit results,
// reduced modulo 2^64.
func TestAggregateAdditiveResultsWrapsToUint64(t *testing.T) {
	maxUint64 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))
	chunks := [][]byte{[]byte(maxUint64.String()), []byte(maxUint64.String())}

	got, err := aggregateAdditiveResults(chunks)
	if err != nil {
		t.Fatal(err)
	}
	value, ok := new(big.Int).SetString(got, 10)
	if !ok {
		t.Fatalf("aggregate %q is not numeric", got)
	}
	limit := new(big.Int).Lsh(big.NewInt(1), 64)
	if value.Cmp(limit) >= 0 {
		t.Fatalf("aggregate %s is not reduced modulo 2^64", got)
	}
	want := new(big.Int).Sub(limit, big.NewInt(2)) // (2^64-1)+(2^64-1) mod 2^64
	if value.Cmp(want) != 0 {
		t.Fatalf("aggregate = %s, want %s", got, want)
	}
}

// TestAggregateAdditiveResultsMatchesWholeFileValue shows why the reduction is
// required: a naive big.Int sum of two maximal chunk results exceeds 2^64, so
// the response would depend on how the file was split into chunks.
func TestAggregateAdditiveResultsMatchesWholeFileValue(t *testing.T) {
	maxUint64 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))
	naive := new(big.Int).Add(maxUint64, maxUint64)

	got, err := aggregateAdditiveResults([][]byte{[]byte(maxUint64.String()), []byte(maxUint64.String())})
	if err != nil {
		t.Fatal(err)
	}
	if got == naive.String() {
		t.Fatalf("aggregate %s still uses unwrapped big.Int arithmetic", got)
	}
	limit := new(big.Int).Lsh(big.NewInt(1), 64)
	if new(big.Int).Mod(naive, limit).String() != got {
		t.Fatalf("aggregate %s is not the reduced naive sum %s", got, naive.String())
	}
}

func TestAggregateAdditiveResultsRejectsNonNumeric(t *testing.T) {
	if _, err := aggregateAdditiveResults([][]byte{[]byte("12"), []byte("3:7")}); err == nil {
		t.Fatal("expected an error for a non-numeric chunk result")
	}
}

// TestValidateChunksCoverFileRequiresPackingAlignedChunks covers the
// precondition of the aggregation theorem: a chunk boundary that falls inside a
// packing unit makes the per-chunk results incongruent with the whole file.
func TestValidateChunksCoverFileRequiresPackingAlignedChunks(t *testing.T) {
	aligned := []*filer_pb.FileChunk{
		{Offset: 0, Size: 16},
		{Offset: 16, Size: 8},
	}
	if err := validateChunksCoverFile(aligned, 24); err != nil {
		t.Fatalf("word-aligned chunks were rejected: %v", err)
	}

	misaligned := []*filer_pb.FileChunk{
		{Offset: 0, Size: 4},
		{Offset: 4, Size: 20},
	}
	err := validateChunksCoverFile(misaligned, 24)
	if err == nil {
		t.Fatal("a chunk that is not a multiple of the packing unit was accepted")
	}
	if !strings.Contains(err.Error(), "multiples of 8") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestMergeTopKResultsPicksGlobalTopK(t *testing.T) {
	bodies := [][]byte{
		[]byte("3:10,7:40,9:90"),
		[]byte("1:5,4:40"),
	}
	got, err := mergeTopKResults(bodies, 3)
	if err != nil {
		t.Fatal(err)
	}
	// 1:5 < 3:10 < 4:40 and 7:40 share a distance, so the smaller index wins.
	if got != "1:5,3:10,4:40" {
		t.Fatalf("merged top-k = %q", got)
	}

	all, err := mergeTopKResults(bodies, 10)
	if err != nil {
		t.Fatal(err)
	}
	if all != "1:5,3:10,4:40,7:40,9:90" {
		t.Fatalf("full merge = %q", all)
	}
}

func TestMergeTopKResultsRejectsMalformedHits(t *testing.T) {
	if _, err := mergeTopKResults([][]byte{[]byte("3-10")}, 3); err == nil {
		t.Fatal("expected an error for a malformed hit")
	}
}

func TestImageTopKFromQuery(t *testing.T) {
	cases := map[string]int{
		"":       defaultImageTopK,
		"1":      1,
		"0":      defaultImageTopK,
		"-3":     defaultImageTopK,
		"abc":    defaultImageTopK,
		"999999": maxImageTopK,
	}
	for raw, want := range cases {
		if got := imageTopKFromQuery(raw); got != want {
			t.Fatalf("imageTopKFromQuery(%q) = %d, want %d", raw, got, want)
		}
	}
}
