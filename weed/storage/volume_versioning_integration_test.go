package storage

import (
	"os"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

// TestComputeRegionReadsRetainedVersion exercises the real path end to end at the
// storage layer: a needle is written through the normal API, a compute region is
// resolved (which is what the volume server sends to the CSD agent), the live
// data file is then replaced the way a compaction commit replaces it, and the
// region must still read the payload that was written.
func TestComputeRegionReadsRetainedVersion(t *testing.T) {
	volume, _ := newVersioningTestVolume(t)

	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i * 7 % 251)
	}
	n := new(needle.Needle)
	n.Id = types.NeedleId(42)
	n.Cookie = types.Cookie(0xB1A11A92)
	n.Data = payload
	n.DataSize = uint32(len(payload))
	n.Checksum = needle.NewCRC(payload)
	if _, _, _, err := volume.writeNeedle2(n, true, false); err != nil {
		t.Fatalf("write needle: %v", err)
	}

	// The volume resolves the compute region for this needle. Under M3 the
	// returned DataFile is a retained version, not the live path.
	region, supported, err := volume.NeedleComputeRegion(n)
	if err != nil || !supported {
		t.Fatalf("NeedleComputeRegion: supported=%v err=%v", supported, err)
	}
	if region.DataFile == volume.FileName(".dat") {
		t.Fatalf("compute region still points at the live data file %s", region.DataFile)
	}
	if region.Generation != volume.CurrentDataGeneration() {
		t.Fatalf("region generation %d, want %d", region.Generation, volume.CurrentDataGeneration())
	}

	readRegion := func() []byte {
		handle, err := os.Open(region.DataFile)
		if err != nil {
			t.Fatalf("open retained version: %v", err)
		}
		defer handle.Close()
		buf := make([]byte, region.DataSize)
		if _, err := handle.ReadAt(buf, region.DataOffset); err != nil {
			t.Fatalf("read retained version: %v", err)
		}
		return buf
	}
	if got := readRegion(); string(got) != string(payload) {
		t.Fatalf("payload read through the version link does not match what was written")
	}

	// Simulate the compaction commit: the live file is replaced by a different
	// inode (in production this is .cpd -> .dat after the old .dat was removed).
	replacement := make([]byte, 8192)
	for i := range replacement {
		replacement[i] = 0xAA
	}
	livePath := volume.FileName(".dat")
	if err := os.Remove(livePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(livePath, replacement, 0644); err != nil {
		t.Fatal(err)
	}

	// The in-flight request keeps reading the generation it started on.
	if got := readRegion(); string(got) != string(payload) {
		t.Fatalf("after the swap the retained version no longer holds the original payload")
	}

	// After the (simulated) compaction the volume moves to a new generation, and
	// the version that was just in use must not be reclaimed immediately.
	volume.bumpDataGeneration()
	if removed := volume.gcExpiredVersions(time.Now(), DefaultVersionRetention, DefaultMaxRetainedVersions); len(removed) != 0 {
		t.Fatalf("reclaimed a version that was just in use: %v", removed)
	}
	if _, err := os.Stat(region.DataFile); err != nil {
		t.Fatalf("retained version vanished while still within the retention window: %v", err)
	}

	// Past the retention window it is reclaimed.
	future := time.Now().Add(2 * DefaultVersionRetention)
	removed := volume.gcExpiredVersions(future, DefaultVersionRetention, 0)
	found := false
	for _, path := range removed {
		if path == region.DataFile {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s to be reclaimed after the retention window, got %v", region.DataFile, removed)
	}
}
