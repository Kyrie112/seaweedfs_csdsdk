package weed_server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/storage"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
)

const csdComputePath = "/v1/compute"

// csdComputeRequest is the SeaweedFS -> CSD agent contract. SeaweedFS passes a
// volume data file and the exact payload range, never the payload itself, so a
// SmartSSD engine can read the range straight into its device buffer and run
// the operator without a temporary file or host-memory copy of the data.
type csdComputeRequest struct {
	Operation string `json:"operation"`
	DataFile  string `json:"data_file"`
	Offset    int64  `json:"offset"`
	Size      int64  `json:"size"`

	// Aligned read window (see storage.ComputeAlignedWindow). A device that can
	// use O_DIRECT reads [ReadOffset, ReadOffset+ReadSize) and skips PayloadShift
	// bytes before packing the payload. An engine that does not understand these
	// fields keeps using Offset/Size: still correct, just through the page cache.
	ReadOffset   int64 `json:"read_offset"`
	ReadSize     int64 `json:"read_size"`
	PayloadShift int64 `json:"payload_shift"`

	// Params carries operator parameters (top-k, record size, query descriptor)
	// from the user request down to the device. Only small control data travels
	// this way - the payload itself is still addressed by file offset, which is
	// what keeps the query image on the client and off the wire.
	Params map[string]string `json:"params,omitempty"`
}

type csdComputeResponse struct {
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`

	// Per-stage device timings and the read path actually taken, reported by
	// the agent (path is one of cache/direct/buffered/legacy).
	ReadMs   int64  `json:"read_ms,omitempty"`
	KernelMs int64  `json:"kernel_ms,omitempty"`
	Direct   bool   `json:"direct,omitempty"`
	Path     string `json:"path,omitempty"`
}

// csdComputeTiming is the device-side cost breakdown of one CSD compute call.
type csdComputeTiming struct {
	Direct   bool
	ReadMs   int64
	KernelMs int64
	Path     string
}

// Response headers used to carry device-side timings back through the volume
// server to the filer. Headers rather than body fields are used so the numeric
// result in the body stays byte-for-byte unchanged for the cross-chunk
// aggregator.
const (
	csdHeaderDirect   = "X-CSD-Direct"
	csdHeaderReadMs   = "X-CSD-Read-Ms"
	csdHeaderKernelMs = "X-CSD-Kernel-Ms"
	csdHeaderPath     = "X-CSD-Path"
)

// csdSupported reports whether the CSD compute path is configured for this
// volume server.
func (vs *VolumeServer) csdSupported() bool {
	return vs.computeConfig.CSDEnabled && vs.computeConfig.CSDEndpoint != ""
}

// csdStatusResponse tells filer schedulers whether this volume server can
// offload compute to a local CSD agent and where that agent listens.
type csdStatusResponse struct {
	CSDEnabled  bool   `json:"csd_enabled"`
	CSDEndpoint string `json:"csd_endpoint,omitempty"`
}

// csdStatusHandler is probed by the filer before choosing which replica should
// execute a CSD-native compute request.
func (vs *VolumeServer) csdStatusHandler(w http.ResponseWriter, r *http.Request) {
	resp := csdStatusResponse{
		CSDEnabled:  vs.csdSupported(),
		CSDEndpoint: vs.computeConfig.CSDEndpoint,
	}
	writeJsonQuiet(w, r, http.StatusOK, resp)
}

// tryHandleCSDCompute tries to serve one needle compute request through the
// configured CSD engine. It returns true only when it wrote a response.
// Any unsupported or failed case returns false so the caller can fall back to
// the existing script+temp-file compute path (single-chunk semantics).
func (vs *VolumeServer) tryHandleCSDCompute(
	w http.ResponseWriter,
	r *http.Request,
	volumeId needle.VolumeId,
	n *needle.Needle,
	expectedCookie types.Cookie,
	operation string,
	filename string,
) bool {
	if !vs.csdSupported() {
		return false
	}
	if r.Method == http.MethodHead {
		return false
	}
	vol := vs.store.GetVolume(volumeId)
	if vol == nil {
		glog.V(4).Infof("CSD skip: volume %d not local", volumeId)
		return false
	}
	region, supported, err := vol.NeedleComputeRegion(n)
	if err != nil {
		glog.V(4).Infof("CSD compute locate needle %d volume %d: %v", n.Id, volumeId, err)
		return false
	}
	if !supported {
		glog.V(4).Infof("CSD skip: needle %d unsupported (maybe compressed/manifest/legacy)", n.Id)
		return false
	}
	if region.Cookie != expectedCookie {
		glog.V(4).Infof("CSD skip: cookie mismatch needle %d", n.Id)
		return false
	}

	result, timing, err := vs.callCSDCompute(r.Context(), operation, region, csdOperatorParams(r))
	if err != nil {
		glog.Warningf("CSD compute %q volume %d needle %d: %v", operation, volumeId, n.Id, err)
		return false
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(result)))
	w.Header().Set(csdHeaderDirect, strconv.FormatBool(timing.Direct))
	w.Header().Set(csdHeaderReadMs, strconv.FormatInt(timing.ReadMs, 10))
	w.Header().Set(csdHeaderKernelMs, strconv.FormatInt(timing.KernelMs, 10))
	if timing.Path != "" {
		w.Header().Set(csdHeaderPath, timing.Path)
	}
	if _, err := w.Write(result); err != nil {
		glog.V(2).Infof("CSD compute response write error: %v", err)
	}
	glog.V(1).Infof("CSD compute %q on volume %d needle %d data=%s offset=%d size=%d read_offset=%d read_size=%d shift=%d direct=%t read_ms=%d kernel_ms=%d",
		operation, volumeId, n.Id, region.DataFile, region.DataOffset, region.DataSize,
		region.ReadOffset, region.ReadSize, region.PayloadShift,
		timing.Direct, timing.ReadMs, timing.KernelMs)
	return true
}

// csdOperatorParams collects the operator parameters a user attached to the
// compute request, excluding the operation selector itself. They are forwarded
// verbatim to the device so new operators can be added without touching the
// volume server.
func csdOperatorParams(r *http.Request) map[string]string {
	query := r.URL.Query()
	params := make(map[string]string, len(query))
	for name, values := range query {
		if name == volumeComputeQuery || len(values) == 0 {
			continue
		}
		params[name] = values[0]
	}
	if len(params) == 0 {
		return nil
	}
	return params
}

func (vs *VolumeServer) callCSDCompute(ctx context.Context, operation string, region storage.NeedleComputeRegion, params map[string]string) ([]byte, csdComputeTiming, error) {
	timeout := vs.computeConfig.CSDTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(csdComputeRequest{
		Operation:    operation,
		DataFile:     region.DataFile,
		Offset:       region.DataOffset,
		Size:         region.DataSize,
		ReadOffset:   region.ReadOffset,
		ReadSize:     region.ReadSize,
		PayloadShift: region.PayloadShift,
		Params:       params,
	})
	if err != nil {
		return nil, csdComputeTiming{}, err
	}
	req, err := http.NewRequestWithContext(runCtx, http.MethodPost, vs.computeConfig.CSDEndpoint+csdComputePath, bytes.NewReader(body))
	if err != nil {
		return nil, csdComputeTiming{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := util_http.GetGlobalHttpClient().Do(req)
	if err != nil {
		return nil, csdComputeTiming{}, fmt.Errorf("csd agent request: %w", err)
	}
	defer util_http.CloseResponse(resp)

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, csdComputeTiming{}, fmt.Errorf("csd agent response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, csdComputeTiming{}, fmt.Errorf("csd agent status %d: %s", resp.StatusCode, string(raw))
	}
	var parsed csdComputeResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, csdComputeTiming{}, fmt.Errorf("csd agent bad json: %w", err)
	}
	if parsed.Error != "" {
		return nil, csdComputeTiming{}, errors.New(parsed.Error)
	}
	timing := csdComputeTiming{
		Direct:   parsed.Direct,
		ReadMs:   parsed.ReadMs,
		KernelMs: parsed.KernelMs,
		Path:     parsed.Path,
	}
	return []byte(parsed.Result), timing, nil
}
