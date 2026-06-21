package weed_server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

type VolumeComputeConfig struct {
	Enabled     bool
	ScriptDir   string
	Timeout     time.Duration
	MaxOutputMB int
}

const volumeComputeQuery = "compute"

func (vs *VolumeServer) maybeHandleComputeOperation(w http.ResponseWriter, r *http.Request, volumeId needle.VolumeId, n *needle.Needle, operation string, filename string) bool {
	if operation == "" {
		return false
	}
	if !vs.computeConfig.Enabled {
		writeJsonError(w, r, http.StatusForbidden, errors.New("volume compute is disabled"))
		return true
	}
	if r.Method == http.MethodHead {
		writeJsonError(w, r, http.StatusMethodNotAllowed, errors.New("volume compute does not support HEAD"))
		return true
	}
	if n.IsChunkedManifest() {
		writeJsonError(w, r, http.StatusBadRequest, errors.New("volume compute does not support chunked manifest needles"))
		return true
	}
	if err := validateComputeOperation(operation); err != nil {
		writeJsonError(w, r, http.StatusBadRequest, err)
		return true
	}
	if n.IsCompressed() {
		data, err := util.DecompressData(n.Data)
		if err != nil {
			writeJsonError(w, r, http.StatusInternalServerError, fmt.Errorf("decompress needle data: %w", err))
			return true
		}
		n.Data = data
	}

	output, err := vs.runComputeScript(r.Context(), operation, volumeId, n, filename)
	if err != nil {
		writeJsonError(w, r, http.StatusInternalServerError, err)
		return true
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(output)))
	if _, err := w.Write(output); err != nil {
		glog.V(2).Infof("volume compute response write error: %v", err)
	}
	return true
}

func validateComputeOperation(operation string) error {
	if operation == "" {
		return errors.New("empty compute operation")
	}
	for _, r := range operation {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			continue
		}
		return fmt.Errorf("invalid compute operation %q", operation)
	}
	if strings.Contains(operation, "..") {
		return fmt.Errorf("invalid compute operation %q", operation)
	}
	return nil
}

func (vs *VolumeServer) runComputeScript(ctx context.Context, operation string, volumeId needle.VolumeId, n *needle.Needle, filename string) ([]byte, error) {
	scriptPath, err := vs.computeScriptPath(operation)
	if err != nil {
		return nil, err
	}

	timeout := vs.computeConfig.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	inputFile, inputPath, cleanup, err := createComputeInputFile(n.Data)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	cmd := exec.CommandContext(runCtx, scriptPath, inputPath)
	cmd.Stdin = inputFile
	cmd.Env = append(os.Environ(),
		"SEAWEED_COMPUTE_OPERATION="+operation,
		"SEAWEED_FILE_NAME="+filename,
		"SEAWEED_COMPUTE_INPUT_FILE="+inputPath,
		"SEAWEED_COMPUTE_INPUT_FD=0",
		"SEAWEED_VOLUME_ID="+volumeId.String(),
		"SEAWEED_NEEDLE_ID="+strconv.FormatUint(uint64(n.Id), 10),
		"SEAWEED_NEEDLE_NAME="+string(n.Name),
		"SEAWEED_NEEDLE_MIME="+string(n.Mime),
		"SEAWEED_NEEDLE_SIZE="+strconv.FormatUint(uint64(n.DataSize), 10),
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &stdout, limit: int64(vs.computeMaxOutputBytes())}
	cmd.Stderr = &limitedBuffer{buf: &stderr, limit: 64 * 1024}

	err = cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("compute operation %q timed out after %s", operation, timeout)
	}
	if err != nil {
		stderrText := strings.TrimSpace(stderr.String())
		if stderrText != "" {
			return nil, fmt.Errorf("compute operation %q failed: %w: %s", operation, err, stderrText)
		}
		return nil, fmt.Errorf("compute operation %q failed: %w", operation, err)
	}
	return stdout.Bytes(), nil
}

func createComputeInputFile(data []byte) (*os.File, string, func(), error) {
	inputFile, err := os.CreateTemp("", "seaweed-compute-needle-*")
	if err != nil {
		return nil, "", nil, fmt.Errorf("create compute input file: %w", err)
	}
	cleanup := func() {
		_ = inputFile.Close()
		_ = os.Remove(inputFile.Name())
	}
	if _, err = inputFile.Write(data); err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("write compute input file: %w", err)
	}
	if _, err = inputFile.Seek(0, 0); err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("seek compute input file: %w", err)
	}
	return inputFile, inputFile.Name(), cleanup, nil
}

func (vs *VolumeServer) computeScriptPath(operation string) (string, error) {
	if vs.computeConfig.ScriptDir == "" {
		return "", errors.New("volume compute script directory is not configured")
	}
	dir, err := filepath.Abs(vs.computeConfig.ScriptDir)
	if err != nil {
		return "", err
	}
	candidates := []string{
		filepath.Join(dir, operation),
		filepath.Join(dir, operation+".sh"),
	}
	for _, candidate := range candidates {
		candidateAbs, err := filepath.Abs(candidate)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(dir, candidateAbs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return "", fmt.Errorf("compute script %q escapes script directory", operation)
		}
		info, err := os.Stat(candidateAbs)
		if err == nil && !info.IsDir() {
			return candidateAbs, nil
		}
	}
	return "", fmt.Errorf("compute script for operation %q not found", operation)
}

func (vs *VolumeServer) computeMaxOutputBytes() int {
	if vs.computeConfig.MaxOutputMB <= 0 {
		return 64 * 1024 * 1024
	}
	return vs.computeConfig.MaxOutputMB * 1024 * 1024
}

type limitedBuffer struct {
	buf   *bytes.Buffer
	limit int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit >= 0 && int64(b.buf.Len()+len(p)) > b.limit {
		allowed := int(b.limit) - b.buf.Len()
		if allowed > 0 {
			_, _ = b.buf.Write(p[:allowed])
		}
		return 0, fmt.Errorf("output exceeded %d bytes", b.limit)
	}
	return b.buf.Write(p)
}
