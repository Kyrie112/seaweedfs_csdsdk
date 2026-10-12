package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
)

func newVersioningTestVolume(t *testing.T) (*Volume, string) {
	t.Helper()
	dir := t.TempDir()
	volume, err := NewVolume(dir, dir, "", needle.VolumeId(1), NeedleMapInMemory,
		&super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.GetCurrentVersion(), 0, 0)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	return volume, dir
}

// TestVersionLinkSurvivesDataFileSwap is the core M3 property: once a compute
// request has taken a version of the data file, replacing the live .dat (which
// is exactly what a compaction commit does) must not change what that version
// reads.
func TestVersionLinkSurvivesDataFileSwap(t *testing.T) {
	volume, dir := newVersioningTestVolume(t)
	livePath := volume.FileName(".dat")

	original := []byte("original generation payload")
	if err := os.WriteFile(livePath, original, 0644); err != nil {
		t.Fatal(err)
	}

	// A compute request retains the current generation and is told to read it.
	versionPath, err := volume.ensureVersionLink()
	if err != nil {
		t.Fatalf("ensureVersionLink: %v", err)
	}
	if !strings.HasSuffix(versionPath, ".dat"+versionFileSuffix+"0") {
		t.Fatalf("unexpected version path %q", versionPath)
	}
	if _, err := os.Stat(versionPath); err != nil {
		t.Fatalf("version file missing: %v", err)
	}
	volume.noteVersionUse(volume.CurrentDataGeneration())

	// Simulate the compaction commit: the live .dat is replaced by a different
	// inode (the compacted copy), while the version link keeps the old one.
	replacement := []byte("compacted generation payload !!")
	if err := os.Remove(livePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(livePath, replacement, 0644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(versionPath)
	if err != nil {
		t.Fatalf("reading the retained version after the swap: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("retained version changed under us: got %q, want %q", got, original)
	}
	// And the live path really is the new data (so the test is not vacuous).
	if live, _ := os.ReadFile(livePath); string(live) != string(replacement) {
		t.Fatalf("live data was not replaced: %q", live)
	}
	_ = dir
}

// TestVersionsAreNotReclaimedWhileInUse: a version used recently must survive,
// and the current generation is never reclaimed.
func TestVersionsAreNotReclaimedWhileInUse(t *testing.T) {
	volume, _ := newVersioningTestVolume(t)
	livePath := volume.FileName(".dat")
	if err := os.WriteFile(livePath, []byte("current"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := volume.ensureVersionLink(); err != nil {
		t.Fatal(err)
	}
	volume.noteVersionUse(volume.CurrentDataGeneration())

	// Move to a new generation and create its version link too.
	volume.bumpDataGeneration()
	if _, err := volume.ensureVersionLink(); err != nil {
		t.Fatal(err)
	}
	current := volume.CurrentDataGeneration()

	reclaim := volume.versionsToReclaim(time.Now(), DefaultVersionRetention, 0)
	if len(reclaim) != 0 {
		t.Fatalf("reclaimed versions that are still within the retention window: %v", reclaim)
	}
	for _, path := range reclaim {
		if strings.HasSuffix(path, versionFileSuffix+string(rune('0'+current))) {
			t.Fatalf("current generation offered for reclaim: %s", path)
		}
	}
}

// TestExpiredVersionsAreReclaimed: past the retention window, and beyond the
// "keep at most N" bound, old versions are actually removed.
func TestExpiredVersionsAreReclaimed(t *testing.T) {
	volume, _ := newVersioningTestVolume(t)
	if err := os.WriteFile(volume.FileName(".dat"), []byte("current"), 0644); err != nil {
		t.Fatal(err)
	}

	// Three historical generations, all with fresh links.
	var historical []string
	for i := 0; i < 3; i++ {
		if _, err := volume.ensureVersionLink(); err != nil {
			t.Fatal(err)
		}
		historical = append(historical, volume.versionFilePath(volume.CurrentDataGeneration()))
		volume.bumpDataGeneration()
	}
	if _, err := volume.ensureVersionLink(); err != nil {
		t.Fatal(err)
	}

	// Nothing is expired yet.
	if removed := volume.gcExpiredVersions(time.Now(), DefaultVersionRetention, DefaultMaxRetainedVersions); len(removed) != 0 {
		t.Fatalf("premature reclaim: %v", removed)
	}

	// With a "now" far in the future and no retention quota, everything except the
	// current generation is reclaimable.
	future := time.Now().Add(24 * time.Hour)
	reclaimable := volume.versionsToReclaim(future, DefaultVersionRetention, 0)
	if len(reclaimable) != len(historical) {
		t.Fatalf("expected %d reclaimable versions, got %d: %v", len(historical), len(reclaimable), reclaimable)
	}
	removed := volume.gcExpiredVersions(future, DefaultVersionRetention, 0)
	for _, path := range removed {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("version file still present after reclaim: %s", path)
		}
	}
	if len(removed) != len(historical) {
		t.Fatalf("reclaimed %d versions, want %d", len(removed), len(historical))
	}
	// The current generation's link must still be there.
	if _, err := os.Stat(volume.versionFilePath(volume.CurrentDataGeneration())); err != nil {
		t.Fatalf("current generation was reclaimed: %v", err)
	}
}

// TestVersionFilesLookNothingLikeVolumes: the version suffix must not make the
// file look like a volume to the disk scanner (.idx / .vif).
func TestVersionFilesLookNothingLikeVolumes(t *testing.T) {
	volume, _ := newVersioningTestVolume(t)
	path := volume.versionFilePath(7)
	if !strings.HasSuffix(path, ".dat"+versionFileSuffix+"7") {
		t.Fatalf("unexpected version file name %q", path)
	}
	if isValidVolume(filepath.Base(path)) {
		t.Fatalf("version file %q would be loaded as a volume", path)
	}
}

// TestGenerationIsRecoveredAcrossRestart: a restarted volume must not reuse a
// generation that still has a version file on disk.
func TestGenerationIsRecoveredAcrossRestart(t *testing.T) {
	volume, dir := newVersioningTestVolume(t)
	if err := os.WriteFile(volume.FileName(".dat"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := volume.ensureVersionLink(); err != nil {
			t.Fatal(err)
		}
		volume.bumpDataGeneration()
	}
	if _, err := volume.ensureVersionLink(); err != nil {
		t.Fatal(err)
	}
	before := volume.CurrentDataGeneration()

	reopened, err := NewVolume(dir, dir, "", needle.VolumeId(1), NeedleMapInMemory,
		&super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.GetCurrentVersion(), 0, 0)
	if err != nil {
		t.Fatalf("reopen volume: %v", err)
	}
	if got := reopened.CurrentDataGeneration(); got != before {
		t.Fatalf("generation after restart = %d, want %d", got, before)
	}
	if len(reopened.existingVersionGenerations()) == 0 {
		t.Fatal("expected version files to be discoverable after restart")
	}
}
