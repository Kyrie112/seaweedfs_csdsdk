package weed_server

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
	"github.com/seaweedfs/seaweedfs/weed/util/mem"
	"github.com/seaweedfs/seaweedfs/weed/util/request_id"
)

// proxyReadConcurrencyPerVolumeServer limits how many concurrent proxy read
// requests the filer will issue to any single volume server. Without this,
// replication bursts can open hundreds of connections to one volume server,
// causing it to drop connections with "unexpected EOF".
const proxyReadConcurrencyPerVolumeServer = 16

var (
	proxySemaphores sync.Map // host -> chan struct{}
)

func acquireProxySemaphore(ctx context.Context, host string) error {
	v, _ := proxySemaphores.LoadOrStore(host, make(chan struct{}, proxyReadConcurrencyPerVolumeServer))
	sem := v.(chan struct{})
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseProxySemaphore(host string) {
	v, ok := proxySemaphores.Load(host)
	if !ok {
		return
	}
	select {
	case <-v.(chan struct{}):
	default:
		glog.Warningf("proxy semaphore for %s was already empty on release", host)
	}
}

func (fs *FilerServer) proxyToVolumeServer(w http.ResponseWriter, r *http.Request, fileId string) {
	ctx := r.Context()
	urlStrings, err := fs.filer.MasterClient.GetLookupFileIdFunction()(ctx, fileId)
	if err != nil {
		glog.ErrorfCtx(ctx, "locate %s: %v", fileId, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if len(urlStrings) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	proxyReq, err := http.NewRequest(r.Method, urlStrings[rand.IntN(len(urlStrings))], r.Body)
	if err != nil {
		glog.ErrorfCtx(ctx, "NewRequest %s: %v", urlStrings[0], err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Limit concurrent requests per volume server to prevent overload
	volumeHost := proxyReq.URL.Host
	if err := acquireProxySemaphore(ctx, volumeHost); err != nil {
		glog.V(0).InfofCtx(ctx, "proxy to %s cancelled while waiting: %v", volumeHost, err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	defer releaseProxySemaphore(volumeHost)

	proxyReq.Header.Set("Host", r.Host)
	proxyReq.Header.Set("X-Forwarded-For", r.RemoteAddr)
	request_id.InjectToRequest(ctx, proxyReq)

	for header, values := range r.Header {
		for _, value := range values {
			proxyReq.Header.Add(header, value)
		}
	}

	proxyResponse, postErr := util_http.GetGlobalHttpClient().Do(proxyReq)

	if postErr != nil {
		glog.ErrorfCtx(ctx, "post to filer: %v", postErr)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer util_http.CloseResponse(proxyResponse)

	for k, v := range proxyResponse.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(proxyResponse.StatusCode)

	buf := mem.Allocate(128 * 1024)
	defer mem.Free(buf)
	if _, copyErr := io.CopyBuffer(w, proxyResponse.Body, buf); copyErr != nil {
		glog.V(0).InfofCtx(ctx, "proxy copy %s: %v", fileId, copyErr)
	}

}

const (
	computeChunkFanoutConcurrency = 8
	computeChunkMaxResultBytes    = 64 << 20 // bounded by volume -volume.compute.maxOutputMB
)

func resolveComputeChunks(ctx context.Context, fs *FilerServer, entry *filer.Entry) ([]*filer_pb.FileChunk, error) {
	if len(entry.Content) > 0 {
		return nil, fmt.Errorf("compute is only supported for volume-backed files")
	}
	chunks := entry.GetChunks()
	if len(chunks) == 0 {
		return nil, fmt.Errorf("compute is only supported for volume-backed files")
	}
	dataChunks, _, err := filer.ResolveChunkManifest(ctx, fs.filer.MasterClient.GetLookupFileIdFunction(), chunks, 0, int64(entry.FileSize))
	if err != nil {
		return nil, fmt.Errorf("resolve chunk manifest: %w", err)
	}
	if len(dataChunks) == 0 {
		return nil, fmt.Errorf("compute target has no data chunks")
	}
	for _, chunk := range dataChunks {
		if len(chunk.GetCipherKey()) > 0 || chunk.GetSseType() != 0 {
			return nil, fmt.Errorf("compute is not supported for encrypted chunks")
		}
		if chunk.GetFileIdString() == "" {
			return nil, fmt.Errorf("compute target chunk has empty file id")
		}
	}
	return dataChunks, nil
}

// validateChunksCoverFile checks that data chunks tile the whole file
// contiguously from offset 0 (no gaps/overlaps), which is required for
// correct cross-chunk aggregation of byte-aligned operators.
func validateChunksCoverFile(chunks []*filer_pb.FileChunk, fileSize int64) error {
	ordered := make([]*filer_pb.FileChunk, len(chunks))
	copy(ordered, chunks)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].GetOffset() < ordered[j].GetOffset() })
	next := int64(0)
	for _, c := range ordered {
		if c.GetOffset() != next {
			return fmt.Errorf("compute requires contiguous chunks without gaps: chunk offset=%d expected=%d", c.GetOffset(), next)
		}
		// Per-chunk results of a packing operator are only congruent with the
		// whole-file result when no chunk boundary falls inside a packing unit.
		if size := int64(c.GetSize()); size%computeChunkPackingUnit != 0 {
			return fmt.Errorf("compute requires chunk sizes to be multiples of %d bytes so per-chunk results stay congruent with the whole-file result: chunk offset=%d size=%d",
				computeChunkPackingUnit, c.GetOffset(), size)
		}
		next = c.GetOffset() + int64(c.GetSize())
	}
	if next != fileSize {
		return fmt.Errorf("compute chunks do not cover the whole file: covered=%d fileSize=%d", next, fileSize)
	}
	return nil
}

func (fs *FilerServer) proxyComputeToVolumeServer(w http.ResponseWriter, r *http.Request, entry *filer.Entry, operation string) {
	ctx := r.Context()
	level := consistencyLevelFromQuery(r.URL.Query().Get("consistency"))
	attempts := computeSnapshotAttempts
	if strings.EqualFold(r.URL.Query().Get(computeQueryParamOnStale), "fail") {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// T2.3: a chunk reported stale_range, which means the data changed
			// after this request planned its version. Re-read the file entry so
			// the next attempt works on a fresh chunk list and snapshot token.
			refreshed, refreshErr := fs.filer.FindEntry(ctx, entry.FullPath)
			if refreshErr != nil || refreshed == nil {
				glog.WarningfCtx(ctx, "compute %q: cannot refresh entry after stale_range: %v", operation, refreshErr)
				break
			}
			glog.V(1).InfofCtx(ctx, "compute %q: retrying with a refreshed chunk list after stale_range", operation)
			entry = refreshed
			w.Header().Set(consistencyHeaderRetry, string(classStaleRange))
		}

		dataChunks, err := resolveComputeChunks(ctx, fs, entry)
		if err != nil {
			writeJsonError(w, r, http.StatusBadRequest, err)
			return
		}
		// T1.2: one snapshot token per request, generated from the chunk list that
		// was just validated, so every chunk shares the same version view.
		snapshot := newComputeSnapshot(string(entry.FullPath), entry, dataChunks)
		w.Header().Set(consistencyHeaderSnapshot, snapshot.digest)
		w.Header().Set(consistencyHeaderLevel, level)
		preferred := newHostPreference()

		var attemptErr error
		if len(dataChunks) == 1 && dataChunks[0].GetOffset() == 0 && dataChunks[0].GetSize() == entry.FileSize {
			// Original single-chunk fast path: whole file lives in one needle.
			attemptErr = fs.proxySingleChunkCompute(w, r, entry, dataChunks[0], snapshot, 0, operation, preferred)
		} else {
			if err := validateChunksCoverFile(dataChunks, int64(entry.FileSize)); err != nil {
				writeJsonError(w, r, http.StatusBadRequest, err)
				return
			}
			glog.V(0).InfofCtx(ctx, "compute %q across %d chunks of %s", operation, len(dataChunks), entry.Name())
			attemptErr = fs.proxyMultiChunkCompute(w, r, entry, dataChunks, snapshot, operation, preferred)
		}
		if attemptErr == nil {
			return // the response has already been written
		}

		// Only a stale range is worth re-planning; node failures were already
		// retried on other replicas inside the chunk dispatch.
		lastErr = attemptErr
		if classifyComputeError(attemptErr) != classStaleRange {
			break
		}
	}

	class := classifyComputeError(lastErr)
	w.Header().Set(consistencyHeaderError, string(class))
	status := http.StatusInternalServerError
	switch class {
	case classStaleRange:
		status = http.StatusConflict
	case classNotFound:
		status = http.StatusNotFound
	case classUnsupported:
		status = http.StatusBadRequest
	}
	glog.WarningfCtx(ctx, "compute %q failed (%s): %v", operation, class, lastErr)
	writeJsonError(w, r, status, lastErr)
}

// chunkComputeTiming captures the filer-side and device-side cost of one chunk
// dispatch: how long the filer needed to get the chunk to a replica, and how
// long the device then spent reading the range and running the kernel.
type chunkComputeTiming struct {
	FileId       string
	PayloadBytes int64
	DispatchMs   int64
	Direct       bool
	DeviceReadMs int64
	KernelMs     int64
	Path         string
	// Snapshot is the token echoed by the device for this chunk; an empty value
	// means the peer did not implement the token (an older agent).
	Snapshot   string
	ChunkIndex int
	// DataVersion is the version the replica reported for the needle it served.
	DataVersion string
}

// computeSnapshotAttempts bounds the request-level retry loop (M2/T2.3):
// the first attempt plus one retry after re-reading the file entry.
const computeSnapshotAttempts = 2

// computeTimingSummary aggregates per-chunk timings for logging and for the
// response headers consumed by the benchmark scripts.
type computeTimingSummary struct {
	Chunks       int
	DirectHits   int
	CacheHits    int
	DispatchMs   int64
	DeviceReadMs int64
	KernelMs     int64
	PayloadBytes int64
}

func summarizeComputeTiming(timings []chunkComputeTiming) computeTimingSummary {
	summary := computeTimingSummary{Chunks: len(timings)}
	for _, timing := range timings {
		if timing.Direct {
			summary.DirectHits++
		}
		if timing.Path == "cache" {
			summary.CacheHits++
		}
		summary.DispatchMs += timing.DispatchMs
		summary.DeviceReadMs += timing.DeviceReadMs
		summary.KernelMs += timing.KernelMs
		summary.PayloadBytes += timing.PayloadBytes
	}
	return summary
}

// setComputeTimingHeaders exposes the per-chunk breakdown to clients. These are
// the numbers used to draw the phase-1 latency waterfall: dispatch (filer side,
// aggregated over the fan-out) versus device read and kernel time.
func setComputeTimingHeaders(w http.ResponseWriter, timings []chunkComputeTiming) {
	if len(timings) == 0 {
		return
	}
	summary := summarizeComputeTiming(timings)
	w.Header().Set("X-Compute-Chunks", strconv.Itoa(summary.Chunks))
	w.Header().Set("X-Compute-Dispatch-Ms", strconv.FormatInt(summary.DispatchMs, 10))
	w.Header().Set("X-Compute-Device-Read-Ms", strconv.FormatInt(summary.DeviceReadMs, 10))
	w.Header().Set("X-Compute-Kernel-Ms", strconv.FormatInt(summary.KernelMs, 10))
	w.Header().Set("X-Compute-Direct-Hits", strconv.Itoa(summary.DirectHits))
	w.Header().Set("X-Compute-Cache-Hits", strconv.Itoa(summary.CacheHits))
	w.Header().Set("X-Compute-Payload-Bytes", strconv.FormatInt(summary.PayloadBytes, 10))
}

func parseHeaderInt64(value string) int64 {
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// proxySingleChunkCompute keeps the original behavior: proxy the compute
// request to the single volume server holding the whole file.
func (fs *FilerServer) proxySingleChunkCompute(w http.ResponseWriter, r *http.Request, entry *filer.Entry, chunk *filer_pb.FileChunk, snapshot computeSnapshot, chunkIndex int, operation string, preferred *hostPreference) error {
	ctx := r.Context()
	fileId := chunk.GetFileIdString()
	body, timing, err := fs.fetchChunkComputeResult(ctx, r, fileId, entry.Name(), operation, int64(chunk.GetSize()), chunk.GetOffset(), snapshot, chunkIndex, preferred)
	if err != nil {
		glog.ErrorfCtx(ctx, "compute proxy to volume: %v", err)
		return err
	}
	setComputeTimingHeaders(w, []chunkComputeTiming{timing})
	glog.V(1).InfofCtx(ctx, "compute %q single chunk %s: dispatch_ms=%d direct=%t device_read_ms=%d kernel_ms=%d",
		operation, fileId, timing.DispatchMs, timing.Direct, timing.DeviceReadMs, timing.KernelMs)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if _, err := w.Write(body); err != nil {
		glog.V(2).InfofCtx(ctx, "compute response write error: %v", err)
	}
	return nil
}

// fetchChunkComputeResult issues one compute request to the volume server that
// owns the given fileId and returns the raw result body plus its timing
// breakdown.
//
// Scheduling: every attempt ranks replicas with the CSD-aware objective (CSD
// capability, least in-flight bytes, fewest in-flight requests, probe latency,
// deterministic host) and reserves the chosen replica's load before dispatch.
// A replica that fails is added to an exclusion set, so the next attempt is
// guaranteed to target a different replica. Without that set, re-ranking could
// hand the request straight back to the replica that just failed and failover
// would be a no-op.
func (fs *FilerServer) fetchChunkComputeResult(ctx context.Context, r *http.Request, fileId string, filename string, operation string, payloadBytes int64, baseOffset int64, snapshot computeSnapshot, chunkIndex int, preferred *hostPreference) (body []byte, timing chunkComputeTiming, err error) {
	timing = chunkComputeTiming{FileId: fileId, PayloadBytes: payloadBytes, ChunkIndex: chunkIndex}
	started := time.Now()
	defer func() {
		timing.DispatchMs = time.Since(started).Milliseconds()
	}()

	urlStrings, err := fs.filer.MasterClient.GetLookupFileIdFunction()(ctx, fileId)
	if err != nil {
		return nil, timing, fmt.Errorf("locate compute target %s: %w", fileId, err)
	}
	if len(urlStrings) == 0 {
		return nil, timing, fmt.Errorf("no volume server for compute target %s", fileId)
	}

	excluded := make(map[string]struct{})
	var lastErr error
	for attempt := 0; attempt < len(urlStrings); attempt++ {
		raw, release, reserveErr := fs.rankAndReserve(ctx, urlStrings, payloadBytes, excluded, preferred)
		if reserveErr != nil {
			if lastErr != nil {
				break
			}
			return nil, timing, reserveErr
		}
		chunkBody, deviceTiming, fetchErr := fs.fetchFromComputeReplica(ctx, r, fileId, filename, operation, raw, baseOffset, snapshot, chunkIndex, payloadBytes)
		release()
		if fetchErr == nil {
			timing.Direct = deviceTiming.Direct
			timing.DeviceReadMs = deviceTiming.ReadMs
			timing.KernelMs = deviceTiming.KernelMs
			timing.Path = deviceTiming.Path
			timing.Snapshot = deviceTiming.Snapshot
			timing.DataVersion = deviceTiming.DataVersion
			// Remember the host so the rest of this request can stay on the same
			// replica set (M2/T2.1).
			preferred.remember(csdHostOf(raw))
			return chunkBody, timing, nil
		}
		// M2/T2.4: classify the failure. A node error or a replica that does not
		// have the chunk yet is worth trying elsewhere; a stale range is not -
		// the data itself changed, so the request level has to re-plan.
		class := classifyComputeError(fetchErr)
		excluded[csdHostOf(raw)] = struct{}{}
		if !class.retryOnAnotherReplica() {
			glog.Warningf("compute %q via %s failed (%s), not retrying another replica: %v",
				operation, raw, class, fetchErr)
			return nil, timing, fetchErr
		}
		glog.Warningf("compute %q via %s failed (%s), trying another replica: %v", operation, raw, class, fetchErr)
		lastErr = fetchErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("compute target %s failed on all replicas", fileId)
	}
	return nil, timing, lastErr
}

func (fs *FilerServer) fetchFromComputeReplica(ctx context.Context, r *http.Request, fileId string, filename string, operation string, replicaURL string, baseOffset int64, snapshot computeSnapshot, chunkIndex int, payloadBytes int64) ([]byte, csdComputeTiming, error) {
	var noTiming csdComputeTiming
	target, err := url.Parse(replicaURL)
	if err != nil {
		return nil, noTiming, err
	}
	if filename != "" {
		target.Path = volumeComputePath(fileId, filename)
	}
	query := r.URL.Query()
	query.Set(volumeComputeQuery, operation)
	// The device only knows the physical byte range inside a volume data file.
	// Operators that carry file-level semantics (record indices, for example)
	// need the logical position of this chunk inside the file, so the filer -
	// the only component that sees the whole chunk list - hands it down per
	// chunk.
	if operation == imageSearchOperation {
		query.Set("base_off", strconv.FormatInt(baseOffset, 10))
	}
	// T1.2: push the snapshot token (stable keys) down to the volume and device.
	for key, value := range snapshot.tokenParams(chunkIndex, payloadBytes) {
		query.Set(key, value)
	}
	target.RawQuery = query.Encode()

	proxyReq, err := http.NewRequestWithContext(ctx, r.Method, target.String(), nil)
	if err != nil {
		return nil, noTiming, err
	}
	proxyReq.Header.Set("Host", r.Host)
	proxyReq.Header.Set("X-Forwarded-For", r.RemoteAddr)
	if jwt := fs.maybeGetVolumeReadJwtAuthorizationToken(fileId); jwt != "" {
		proxyReq.Header.Set("Authorization", "Bearer "+jwt)
	}
	request_id.InjectToRequest(ctx, proxyReq)
	for header, values := range r.Header {
		if strings.EqualFold(header, "Authorization") {
			continue
		}
		for _, value := range values {
			proxyReq.Header.Add(header, value)
		}
	}

	volumeHost := proxyReq.URL.Host
	if err := acquireProxySemaphore(ctx, volumeHost); err != nil {
		return nil, noTiming, fmt.Errorf("compute proxy to %s cancelled while waiting: %w", volumeHost, err)
	}
	defer releaseProxySemaphore(volumeHost)

	proxyResponse, postErr := util_http.GetGlobalHttpClient().Do(proxyReq)
	if postErr != nil {
		return nil, noTiming, fmt.Errorf("compute proxy to volume: %w", postErr)
	}
	defer util_http.CloseResponse(proxyResponse)
	if proxyResponse.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(proxyResponse.Body, 4096))
		if proxyResponse.StatusCode == http.StatusConflict {
			// The volume detected a stale range (data changed under us). This is
			// not a node failure, so it must keep its meaning all the way up.
			return nil, noTiming, fmt.Errorf("%w: %s", errStaleRange, strings.TrimSpace(string(errBody)))
		}
		return nil, noTiming, fmt.Errorf("volume compute returned status %d: %s", proxyResponse.StatusCode, strings.TrimSpace(string(errBody)))
	}
	timing := csdComputeTiming{
		Direct:      strings.EqualFold(proxyResponse.Header.Get(csdHeaderDirect), "true"),
		ReadMs:      parseHeaderInt64(proxyResponse.Header.Get(csdHeaderReadMs)),
		KernelMs:    parseHeaderInt64(proxyResponse.Header.Get(csdHeaderKernelMs)),
		Path:        proxyResponse.Header.Get(csdHeaderPath),
		Snapshot:    proxyResponse.Header.Get(consistencyHeaderSnapshot),
		Stale:       strings.EqualFold(proxyResponse.Header.Get(csdHeaderStaleRange), "true"),
		DataVersion: proxyResponse.Header.Get(csdHeaderDataVersion),
	}
	body, err := io.ReadAll(io.LimitReader(proxyResponse.Body, computeChunkMaxResultBytes))
	if err != nil {
		return nil, noTiming, fmt.Errorf("read volume compute result: %w", err)
	}
	return body, timing, nil
}

// proxyMultiChunkCompute fans the compute request out to every chunk's
// volume server (each chunk is computed in place on its volume), then
// aggregates the small numeric per-chunk results at the filer and reports
// the merged total back to the client.
func (fs *FilerServer) proxyMultiChunkCompute(w http.ResponseWriter, r *http.Request, entry *filer.Entry, chunks []*filer_pb.FileChunk, snapshot computeSnapshot, operation string, preferred *hostPreference) error {
	ctx := r.Context()
	type chunkResult struct {
		body   []byte
		timing chunkComputeTiming
		err    error
	}
	results := make([]chunkResult, len(chunks))
	sem := make(chan struct{}, computeChunkFanoutConcurrency)
	started := time.Now()
	var wg sync.WaitGroup
	for i := range chunks {
		fileId := chunks[i].GetFileIdString()
		payloadBytes := int64(chunks[i].GetSize())
		wg.Add(1)
		go func(idx int, fid string, payload int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			body, timing, err := fs.fetchChunkComputeResult(ctx, r, fid, entry.Name(), operation, payload, chunks[idx].GetOffset(), snapshot, idx, preferred)
			results[idx] = chunkResult{body: body, timing: timing, err: err}
		}(i, fileId, payloadBytes)
	}
	wg.Wait()

	timings := make([]chunkComputeTiming, 0, len(results))
	for i := range results {
		timings = append(timings, results[i].timing)
	}

	// T1.3: every chunk must echo the same snapshot token. A mismatch means the
	// chunks are not all from the version this request planned to compute over.
	level := consistencyLevelFromQuery(r.URL.Query().Get("consistency"))
	echoed := make([]string, 0, len(timings))
	indexes := make([]string, 0, len(timings))
	for _, t := range timings {
		echoed = append(echoed, t.Snapshot)
		indexes = append(indexes, strconv.Itoa(t.ChunkIndex))
	}
	effectiveLevel, staleChunks, ok := verifyChunkTokens(level, snapshot, echoed, indexes, len(chunks))
	w.Header().Set(consistencyHeaderLevel, effectiveLevel)
	w.Header().Set(consistencyHeaderStale, strconv.Itoa(staleChunks))
	if !ok {
		return fmt.Errorf("%w: %d of %d chunks did not confirm snapshot %s",
			errStaleRange, staleChunks, len(chunks), snapshot.digest)
	}
	if staleChunks > 0 {
		glog.Warningf("compute %q: %d of %d chunks did not confirm snapshot %s; result is marked %s",
			operation, staleChunks, len(chunks), snapshot.digest, effectiveLevel)
	}
	summary := summarizeComputeTiming(timings)
	setComputeTimingHeaders(w, timings)
	glog.V(1).InfofCtx(ctx, "compute %q across %d chunks: wall_ms=%d sum_dispatch_ms=%d fanout=%d csd_direct_hits=%d/%d csd_cache_hits=%d/%d device_read_ms=%d kernel_ms=%d payload_bytes=%d",
		operation, len(chunks), time.Since(started).Milliseconds(), summary.DispatchMs,
		computeChunkFanoutConcurrency, summary.DirectHits, summary.Chunks,
		summary.CacheHits, summary.Chunks,
		summary.DeviceReadMs, summary.KernelMs, summary.PayloadBytes)

	// Aggregation is operator specific: additive numeric operators combine in
	// the ring of 64-bit integers, ranking operators merge partial top-k lists.
	bodies := make([][]byte, len(results))
	for i := range results {
		if results[i].err != nil {
			// Return the error to the caller: the request level decides whether to
			// re-plan (stale) or surface it with the right status.
			return fmt.Errorf("chunk %d compute failed: %w", i, results[i].err)
		}
		bodies[i] = results[i].body
	}
	var aggregate string
	var aggregateErr error
	if operation == imageSearchOperation {
		aggregate, aggregateErr = mergeTopKResults(bodies, imageTopKFromQuery(r.URL.Query().Get("k")))
	} else {
		aggregate, aggregateErr = aggregateAdditiveResults(bodies)
	}
	if aggregateErr != nil {
		return aggregateErr
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(aggregate)))
	if _, err := io.WriteString(w, aggregate); err != nil {
		glog.V(2).Infof("compute response write error: %v", err)
	}
	return nil
}

func volumeComputePath(fileId string, filename string) string {
	vid, fid, found := strings.Cut(fileId, ",")
	if !found {
		return "/" + fileId
	}
	return "/" + vid + "/" + fid + "/" + url.PathEscape(filename)
}
