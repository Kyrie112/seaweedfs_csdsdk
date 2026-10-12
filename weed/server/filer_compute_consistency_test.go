package weed_server

import (
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

func testChunks() []*filer_pb.FileChunk {
	return []*filer_pb.FileChunk{
		{Offset: 0, Size: 1024, FileId: "1,01"},
		{Offset: 1024, Size: 2048, FileId: "1,02"},
	}
}

func testSnapshot() computeSnapshot {
	entry := &filer.Entry{Chunks: testChunks()}
	entry.Mtime = entry.Mtime.Add(1700000000)
	return newComputeSnapshot("/data/a.bin", entry, testChunks())
}

// TestDigestChunksIsStableAndSensitive: the token must be stable for the same
// chunk list and must change when the list changes.
func TestDigestChunksIsStableAndSensitive(t *testing.T) {
	a := digestChunks(testChunks())
	b := digestChunks(testChunks())
	if a != b {
		t.Fatalf("digest is not stable: %s vs %s", a, b)
	}
	if len(a) != 12 {
		t.Fatalf("digest %q should be a 12 character summary", a)
	}

	changed := testChunks()
	changed[1].Size = 4096
	if digestChunks(changed) == a {
		t.Fatal("digest did not change when a chunk size changed")
	}

	reordered := []*filer_pb.FileChunk{testChunks()[1], testChunks()[0]}
	if digestChunks(reordered) == a {
		t.Fatal("digest did not change when the chunk order changed")
	}
}

// TestVerifyChunkTokens covers the three outcomes of the pre-aggregation check.
func TestVerifyChunkTokens(t *testing.T) {
	snapshot := testSnapshot()

	// all chunks confirm the token: level is kept
	level, stale, ok := verifyChunkTokens(consistencyReadCommitted, snapshot,
		[]string{snapshot.digest, snapshot.digest}, []string{"0", "1"}, 2)
	if !ok || stale != 0 || level != consistencyReadCommitted {
		t.Fatalf("confirmed tokens: level=%s stale=%d ok=%v", level, stale, ok)
	}

	// one chunk does not confirm: L1 still returns the result but marks it L0
	level, stale, ok = verifyChunkTokens(consistencyReadCommitted, snapshot,
		[]string{snapshot.digest, "deadbeefdead"}, []string{"0", "1"}, 2)
	if !ok || stale != 1 || level != consistencyBestEffort {
		t.Fatalf("L1 with a mismatching chunk: level=%s stale=%d ok=%v", level, stale, ok)
	}

	// same input at L2 must fail
	_, _, ok = verifyChunkTokens(consistencySnapshot, snapshot,
		[]string{snapshot.digest, "deadbeefdead"}, []string{"0", "1"}, 2)
	if ok {
		t.Fatal("L2 accepted a request whose chunks disagree on the snapshot")
	}

	// an older peer that does not echo the token counts as unconfirmed
	level, stale, ok = verifyChunkTokens(consistencyReadCommitted, snapshot,
		[]string{"", ""}, []string{"0", "1"}, 2)
	if !ok || stale != 2 || level != consistencyBestEffort {
		t.Fatalf("missing token echo: level=%s stale=%d ok=%v", level, stale, ok)
	}

	// a missing chunk is counted as unconfirmed too
	_, stale, _ = verifyChunkTokens(consistencyReadCommitted, snapshot,
		[]string{snapshot.digest}, []string{"0"}, 2)
	if stale != 1 {
		t.Fatalf("missing chunk counted as stale=%d, want 1", stale)
	}
}

func TestConsistencyLevelFromQuery(t *testing.T) {
	cases := map[string]string{
		"":   consistencyReadCommitted,
		"L0": consistencyBestEffort,
		"l0": consistencyBestEffort,
		"L1": consistencyReadCommitted,
		"L2": consistencySnapshot,
		"l2": consistencySnapshot,
		"xx": consistencyReadCommitted,
	}
	for raw, want := range cases {
		if got := consistencyLevelFromQuery(raw); got != want {
			t.Fatalf("consistencyLevelFromQuery(%q) = %s, want %s", raw, got, want)
		}
	}
}

// TestSnapshotTokenParamsAreReserved: the keys pushed down to the device are the
// ones the device parses.
func TestSnapshotTokenParamsAreReserved(t *testing.T) {
	snapshot := testSnapshot()
	params := snapshot.tokenParams(3, 4096)
	if params[computeParamSnapshot] != snapshot.digest {
		t.Fatalf("snapshot param = %q", params[computeParamSnapshot])
	}
	if params[computeParamChunkIndex] != "3" || params[computeParamChunkSize] != "4096" {
		t.Fatalf("unexpected params: %v", params)
	}
	for _, key := range []string{computeParamSnapshot, computeParamChunkIndex, computeParamChunkSize,
		computeParamNeedleCookie, computeParamNeedleId} {
		if !strings.HasPrefix(key, "s") && !strings.HasPrefix(key, "c") &&
			!strings.HasPrefix(key, "n") {
			t.Fatalf("reserved key %q looks inconsistent", key)
		}
	}
}
