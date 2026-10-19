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

	// Snapshot 是设备回显的快照摘要;Stale 表示设备侧判定区间已失效。
	Snapshot string `json:"snapshot,omitempty"`
	Stale    bool   `json:"stale,omitempty"`
}

// csdComputeTiming is the device-side cost breakdown of one CSD compute call.
type csdComputeTiming struct {
	Direct      bool
	ReadMs      int64
	KernelMs    int64
	Path        string
	Snapshot    string
	Stale       bool
	DataVersion string
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

// csdVersionsPath 暴露卷上被保留的数据版本(M3),并允许按需回收。
//
// 运维用途:确认"哪些旧版本还在、最近什么时候被用过";在磁盘压力下用较短的
// 保留期提前回收。仅在启用 CSD 计算时注册。
const csdVersionsPath = "/compute/versions"

type csdVersionResponse struct {
	VolumeId   string                    `json:"volume_id"`
	Generation uint64                    `json:"generation"`
	Retention  string                    `json:"retention"`
	Versions   []storage.DataVersionInfo `json:"versions"`
}

// csdVersionsHandler lists the retained data versions; POST runs a collection
// pass with an optional `retention` duration.
func (vs *VolumeServer) csdVersionsHandler(w http.ResponseWriter, r *http.Request) {
	if !vs.csdSupported() {
		writeJsonError(w, r, http.StatusNotFound, errors.New("CSD compute is not enabled"))
		return
	}
	retention := storage.DefaultVersionRetention
	if raw := r.URL.Query().Get("retention"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			retention = parsed
		}
	}
	volumeId := needle.VolumeId(0)
	if raw := r.URL.Query().Get("volume"); raw != "" {
		if parsed, err := needle.NewVolumeId(raw); err == nil {
			volumeId = parsed
		}
	}
	vol := vs.store.GetVolume(volumeId)
	if vol == nil {
		writeJsonError(w, r, http.StatusNotFound, fmt.Errorf("volume %d is not local", volumeId))
		return
	}
	if r.Method == http.MethodPost {
		switch r.URL.Query().Get("action") {
		case "lease":
			// Hold a version for an explicit (test/ops) period.
			generation, _ := strconv.ParseUint(r.URL.Query().Get("generation"), 10, 64)
			leaseTTL := retention
			if raw := r.URL.Query().Get("ttl"); raw != "" {
				if parsed, err := time.ParseDuration(raw); err == nil {
					leaseTTL = parsed
				}
			}
			lease := vol.AcquireVersionLease(generation, leaseTTL)
			glog.V(0).Infof("volume %d: acquired version lease %d on generation %d for %s",
				volumeId, lease, generation, leaseTTL)
			writeJsonQuiet(w, r, http.StatusOK, map[string]any{
				"volume_id": volumeId.String(), "lease": lease,
				"generation": generation, "ttl": leaseTTL.String(),
			})
			return
		case "release":
			lease, _ := strconv.ParseUint(r.URL.Query().Get("lease"), 10, 64)
			vol.ReleaseVersionLease(lease)
			glog.V(0).Infof("volume %d: released version lease %d", volumeId, lease)
			writeJsonQuiet(w, r, http.StatusOK, map[string]any{
				"volume_id": volumeId.String(), "released": lease,
			})
			return
		default:
			removed := vol.GarbageCollectDataVersionsWithRetention(retention, storage.DefaultMaxRetainedVersions)
			glog.V(0).Infof("volume %d: reclaimed %d data version(s) with retention %s", volumeId, len(removed), retention)
			writeJsonQuiet(w, r, http.StatusOK, map[string]any{
				"volume_id": volumeId.String(), "reclaimed": removed,
				"retention": retention.String(), "generation": vol.CurrentDataGeneration(),
			})
			return
		}
	}
	writeJsonQuiet(w, r, http.StatusOK, csdVersionResponse{
		VolumeId:   volumeId.String(),
		Generation: vol.CurrentDataGeneration(),
		Retention:  retention.String(),
		Versions:   vol.DataVersionInfos(),
	})
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

	params := csdOperatorParams(r, n)

	// T1.4 一致性前置校验:filer 期望的该块长度必须与本地 needle 的数据长度一致。
	// 不一致意味着"描述符生成之后数据被改写/替换",此时**不能**降级到脚本路径
	// (那会算到另一份数据),必须显式失败并让上层决定是否重试。
	if expected, ok := params[computeParamChunkSize]; ok {
		if want, parseErr := strconv.ParseInt(expected, 10, 64); parseErr == nil && want != region.DataSize {
			glog.Warningf("CSD compute %q volume %d needle %d: %s: filer expects %d bytes, needle has %d",
				operation, volumeId, n.Id, staleRangeErrorCode, want, region.DataSize)
			w.Header().Set(csdHeaderErrorClass, string(classStaleRange))
			writeJsonError(w, r, http.StatusConflict, fmt.Errorf(
				"%s: chunk size changed (filer expects %d bytes, needle %d now holds %d)",
				staleRangeErrorCode, want, n.Id, region.DataSize))
			return true
		}
	}

	// M3/MVCC: pin the data version for the duration of the device read. The
	// lease is released immediately after the call, so the version cannot be
	// reclaimed while the device is reading it; if this process dies, the lease
	// expires by TTL.
	leaseID := vol.AcquireVersionLease(region.Generation, storage.DefaultVersionLeaseTTL)
	result, timing, err := vs.callCSDCompute(r.Context(), operation, region, params)
	vol.ReleaseVersionLease(leaseID)
	if err != nil {
		if errors.Is(err, errStaleRange) {
			// Detectable inconsistency: report it instead of falling back, so a
			// configured compute script cannot silently compute over changed data.
			glog.Warningf("CSD compute %q volume %d needle %d rejected: %v", operation, volumeId, n.Id, err)
			w.Header().Set(csdHeaderStaleRange, "true")
			w.Header().Set(csdHeaderErrorClass, string(classStaleRange))
			writeJsonError(w, r, http.StatusConflict, err)
			return true
		}
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
	if timing.Snapshot != "" {
		w.Header().Set(consistencyHeaderSnapshot, timing.Snapshot)
	}
	w.Header().Set(csdHeaderStaleRange, strconv.FormatBool(timing.Stale))
	// M2/T2.2: report the data version actually served, so the filer can confirm
	// that every replica answered for the needle it planned to read.
	w.Header().Set(csdHeaderDataVersion, dataVersionOf(uint64(n.Id), uint32(n.Cookie), region.DataSize, region.LastModified))
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
//
// The needle identity is added here (not by the client): the device uses it to
// verify that the bytes it is about to read still belong to the object the
// filer planned to compute over.
func csdOperatorParams(r *http.Request, n *needle.Needle) map[string]string {
	query := r.URL.Query()
	params := make(map[string]string, len(query)+2)
	for name, values := range query {
		if name == volumeComputeQuery || len(values) == 0 {
			continue
		}
		params[name] = values[0]
	}
	if n != nil {
		params[computeParamNeedleCookie] = strconv.FormatUint(uint64(n.Cookie), 10)
		params[computeParamNeedleId] = strconv.FormatUint(uint64(n.Id), 10)
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
		// The agent reports a detectable inconsistency with 409 + stale=true.
		var agentErr csdComputeResponse
		if json.Unmarshal(raw, &agentErr) == nil && (agentErr.Stale || resp.StatusCode == http.StatusConflict) {
			return nil, csdComputeTiming{Stale: true, Snapshot: agentErr.Snapshot},
				fmt.Errorf("%w: %s", errStaleRange, agentErr.Error)
		}
		return nil, csdComputeTiming{}, fmt.Errorf("csd agent status %d: %s", resp.StatusCode, string(raw))
	}
	var parsed csdComputeResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, csdComputeTiming{}, fmt.Errorf("csd agent bad json: %w", err)
	}
	if parsed.Error != "" {
		if parsed.Stale {
			return nil, csdComputeTiming{Stale: true, Snapshot: parsed.Snapshot},
				fmt.Errorf("%w: %s", errStaleRange, parsed.Error)
		}
		return nil, csdComputeTiming{}, errors.New(parsed.Error)
	}
	timing := csdComputeTiming{
		Direct:   parsed.Direct,
		ReadMs:   parsed.ReadMs,
		KernelMs: parsed.KernelMs,
		Path:     parsed.Path,
		Snapshot: parsed.Snapshot,
		Stale:    parsed.Stale,
	}
	return []byte(parsed.Result), timing, nil
}
