package weed_server

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
)

func TestValidateComputeOperation(t *testing.T) {
	valid := []string{"sum", "word-count", "image.resize", "op_1"}
	for _, op := range valid {
		if err := validateComputeOperation(op); err != nil {
			t.Fatalf("validateComputeOperation(%q): %v", op, err)
		}
	}

	invalid := []string{"", "../sum", "/bin/sh", `a\b`, "sum;rm"}
	for _, op := range invalid {
		if err := validateComputeOperation(op); err == nil {
			t.Fatalf("validateComputeOperation(%q) succeeded, want error", op)
		}
	}
}

func TestVolumeComputePath(t *testing.T) {
	got := volumeComputePath("5,030ccce603", "big numbers.txt")
	want := "/5/030ccce603/big%20numbers.txt"
	if got != want {
		t.Fatalf("volumeComputePath = %q, want %q", got, want)
	}
}

func TestCreateComputeInputFile(t *testing.T) {
	inputFile, inputPath, cleanup, err := createComputeInputFile([]byte("needle-data"))
	if err != nil {
		t.Fatalf("createComputeInputFile: %v", err)
	}
	if inputPath == "" {
		t.Fatal("inputPath is empty")
	}

	data, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("read input file by path: %v", err)
	}
	if string(data) != "needle-data" {
		t.Fatalf("path data = %q, want needle-data", data)
	}

	fdData := make([]byte, len("needle-data"))
	if _, err := inputFile.Read(fdData); err != nil {
		t.Fatalf("read input file handle: %v", err)
	}
	if string(fdData) != "needle-data" {
		t.Fatalf("fd data = %q, want needle-data", fdData)
	}

	cleanup()
	if _, err := os.Stat(inputPath); !os.IsNotExist(err) {
		t.Fatalf("input file still exists after cleanup, stat err: %v", err)
	}
}

func TestRunComputeScriptPassesInputFileAndMetadata(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script execution test requires /bin/sh")
	}

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "inspect.sh")
	script := `#!/bin/sh
printf "arg=%s\n" "$1"
printf "env=%s\n" "$SEAWEED_COMPUTE_INPUT_FILE"
printf "fd=%s\n" "$SEAWEED_COMPUTE_INPUT_FD"
printf "file=%s\n" "$SEAWEED_FILE_NAME"
printf "path-bytes=%s\n" "$(cat "$SEAWEED_COMPUTE_INPUT_FILE")"
printf "stdin-bytes=%s\n" "$(cat)"
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	vs := &VolumeServer{
		computeConfig: VolumeComputeConfig{
			Enabled:     true,
			ScriptDir:   scriptDir,
			Timeout:     5 * time.Second,
			MaxOutputMB: 1,
		},
	}
	n := &needle.Needle{
		Id:       123,
		Data:     []byte("needle-data"),
		DataSize: uint32(len("needle-data")),
		Name:     []byte("source.txt"),
		Mime:     []byte("text/plain"),
	}

	output, err := vs.runComputeScript(context.Background(), "inspect", 7, n, "file.txt")
	if err != nil {
		t.Fatalf("runComputeScript: %v", err)
	}
	got := string(output)
	for _, want := range []string{
		"fd=0\n",
		"file=file.txt\n",
		"path-bytes=needle-data\n",
		"stdin-bytes=needle-data\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}
