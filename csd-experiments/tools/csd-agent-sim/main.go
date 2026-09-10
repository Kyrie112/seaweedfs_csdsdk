// csd-agent-sim is a software stand-in for the SmartSSD CSD agent.
//
// It speaks the exact same SeaweedFS -> agent contract as
// smartssd_compute_module/src/csd_agent.cpp (POST /v1/compute with a range
// descriptor) and implements the same sum semantics, including the v2
// payload_shift behaviour, but runs on the host CPU so the whole pipeline can
// be deployed and measured on a machine without XRT or an FPGA.
//
// The point of the tool is to make the phase-1 questions measurable:
//
//   - does a real SeaweedFS needle payload actually get read with O_DIRECT,
//     now that the volume server sends an aligned window?
//   - does the aligned window produce bit-identical results to the legacy
//     payload-aligned read?
//   - how do the three read paths compare (legacy / aligned buffered /
//     aligned O_DIRECT)?
//
// Three read modes are supported and selected per deployment via -mode:
//
//	legacy   read exactly [offset,size) with a buffered pread (v1 semantics:
//	         the buffer starts at the payload, so O_DIRECT is impossible)
//	aligned  read the aligned window with a buffered pread, then pack the
//	         payload using payload_shift
//	direct   read the aligned window with O_DIRECT, then pack the payload
//	         using payload_shift (the behaviour the fixed agent has)
//	auto     probe the page cache first and use it on a hit, otherwise fall
//	         back to the direct path (adaptive data path selection)
//
// Endpoints:
//
//	POST /v1/compute  same contract as the device agent
//	GET  /v1/stats    cumulative counters (direct hits/misses, short reads)
//	GET  /v1/last     last range descriptor, for independent verification
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// alignment matches csd::kReadAlignment in src/aligned_range.h.
const alignment = 512

type readMode string

const (
	modeLegacy  readMode = "legacy"
	modeAligned readMode = "aligned"
	modeDirect  readMode = "direct"
	// modeAuto probes the page cache with preadv2(RWF_NOWAIT) first: a
	// successful probe means the data is already resident and the probe read
	// *is* the data transfer, so it costs nothing extra. A miss (EAGAIN) or a
	// partial hit falls back to the aligned O_DIRECT read.
	modeAuto readMode = "auto"
)

// computeRequest mirrors csdComputeRequest in weed/server/csd_compute.go.
type computeRequest struct {
	Operation    string `json:"operation"`
	DataFile     string `json:"data_file"`
	Offset       int64  `json:"offset"`
	Size         int64  `json:"size"`
	ReadOffset   int64  `json:"read_offset"`
	ReadSize     int64  `json:"read_size"`
	PayloadShift int64  `json:"payload_shift"`
}

type computeResponse struct {
	Result    string `json:"result"`
	Error     string `json:"error,omitempty"`
	ReadMs    int64  `json:"read_ms,omitempty"`
	KernelMs  int64  `json:"kernel_ms,omitempty"`
	Direct    bool   `json:"direct,omitempty"`
	ReadBytes int64  `json:"read_bytes,omitempty"`
	Path      string `json:"path,omitempty"`
	ProbeMs   int64  `json:"probe_ms,omitempty"`
}

type window struct {
	readOffset   int64
	readSize     int64
	payloadShift int64
}

// planAlignedWindow mirrors csd::plan_aligned_window.
func planAlignedWindow(dataOffset, dataSize int64) window {
	if dataSize <= 0 {
		return window{readOffset: dataOffset}
	}
	shift := dataOffset % alignment
	if shift < 0 {
		shift += alignment
	}
	span := shift + dataSize
	size := span
	if remainder := span % alignment; remainder != 0 {
		size = span + (alignment - remainder)
	}
	return window{readOffset: dataOffset - shift, readSize: size, payloadShift: shift}
}

func (w window) covers(dataOffset, dataSize int64) bool {
	return w.readSize > 0 && w.readOffset+w.payloadShift == dataOffset &&
		w.payloadShift+dataSize <= w.readSize
}

// sumPayload mirrors the file_sum64 kernel: bytes are packed into little-endian
// uint64 words and the words are added, so byte k of the payload contributes
// 256^(k mod 8). Packing starts at the payload's own first byte.
func sumPayload(buf []byte, shift, size int64) uint64 {
	var sum uint64
	words := (size + 7) / 8
	for i := int64(0); i < words; i++ {
		var word uint64
		for j := int64(0); j < 8; j++ {
			index := i*8 + j
			if index >= size {
				break
			}
			word |= uint64(buf[shift+index]) << (8 * uint(j))
		}
		sum += word
	}
	return sum
}

// alignedBuffer owns a page-aligned region so O_DIRECT can be used.
type alignedBuffer struct {
	region []byte
	slice  []byte
}

func newAlignedBuffer(size int64) (*alignedBuffer, error) {
	region, err := syscall.Mmap(-1, 0, int(size)+4096,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("mmap %d bytes: %w", size, err)
	}
	base := uintptr(unsafe.Pointer(&region[0]))
	offset := int((4096 - base%4096) % 4096)
	return &alignedBuffer{region: region, slice: region[offset : offset+int(size)]}, nil
}

func (b *alignedBuffer) Close() {
	_ = syscall.Munmap(b.region)
}

// readFullyAt loops until the buffer is full, mirroring csd::read_fully_with.
// A short return means EOF or an error and must be rejected by the caller.
func readFullyAt(fd int, buf []byte, offset int64) (int, error) {
	done := 0
	for done < len(buf) {
		n, err := syscall.Pread(fd, buf[done:], offset+int64(done))
		if err != nil {
			return done, err
		}
		if n == 0 {
			break // EOF
		}
		done += n
	}
	return done, nil
}

type agent struct {
	mode      readMode
	evict     bool
	mu        sync.Mutex
	last      computeRequest
	requests  int64
	directHi  int64
	directLo  int64
	short     int64
	readBy    int64
	cacheHit  int64
	cachePart int64
	cacheMiss int64
	probeErr  int64
}

func (a *agent) handleCompute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req computeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, computeResponse{Error: "bad json: " + err.Error()})
		return
	}
	resp, status := a.run(req)
	writeJSON(w, status, resp)
}

func (a *agent) run(req computeRequest) (computeResponse, int) {
	if req.Operation != "rawsum64" && req.Operation != "file_sum64" {
		return computeResponse{Error: "unsupported operation " + req.Operation}, http.StatusBadRequest
	}
	if req.Size == 0 {
		return computeResponse{Result: "0"}, http.StatusOK
	}
	if req.Size < 0 || req.Offset < 0 {
		return computeResponse{Error: "negative offset or size"}, http.StatusBadRequest
	}

	w := planAlignedWindow(req.Offset, req.Size)
	if req.ReadSize > 0 {
		declared := window{req.ReadOffset, req.ReadSize, req.PayloadShift}
		if !declared.covers(req.Offset, req.Size) {
			log.Printf("warning: ignoring inconsistent window from volume server: declared=%+v recomputed=%+v", declared, w)
		}
	}

	useDirect := a.mode == modeDirect || a.mode == modeAuto
	probeCache := a.mode == modeAuto
	if a.mode == modeLegacy {
		// v1 semantics: the buffer must start exactly at the payload.
		w = window{readOffset: req.Offset, readSize: req.Size}
		useDirect = false
		probeCache = false
	}

	buf, err := newAlignedBuffer(w.readSize)
	if err != nil {
		return computeResponse{Error: err.Error()}, http.StatusInternalServerError
	}
	defer buf.Close()

	fd, err := syscall.Open(req.DataFile, syscall.O_RDONLY, 0)
	if err != nil {
		return computeResponse{Error: "open " + req.DataFile + ": " + err.Error()}, http.StatusInternalServerError
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return computeResponse{Error: "stat " + req.DataFile + ": " + err.Error()}, http.StatusInternalServerError
	}
	// The rounded window can run past EOF: a volume .dat file ends exactly at
	// the last needle's last byte. Bytes past EOF are never payload, so only an
	// incomplete *payload* is an error.
	available := w.readOffset + w.readSize
	if available > st.Size {
		available = st.Size
	}
	available -= w.readOffset
	if available < 0 {
		available = 0
	}
	if w.payloadShift+req.Size > available {
		syscall.Close(fd)
		return computeResponse{Error: fmt.Sprintf(
			"needle payload is truncated: %s has %d of %d bytes needed at window offset %d",
			req.DataFile, available, w.payloadShift+req.Size, w.readOffset)}, http.StatusInternalServerError
	}

	if a.evict {
		// Evict just this file from the page cache so the measurement is a cold
		// read. Targeted fadvise avoids dropping the whole host page cache.
		if err := unix.Fadvise(fd, 0, 0, unix.FADV_DONTNEED); err != nil {
			log.Printf("fadvise(DONTNEED) failed: %v", err)
		}
	}

	// O_DIRECT needs whole blocks; a sub-block tail (if any) is read buffered.
	directLen := available - available%alignment
	direct := false
	directFD := -1
	if useDirect && w.readOffset%alignment == 0 && directLen > 0 &&
		uintptr(unsafe.Pointer(&buf.slice[0]))%alignment == 0 {
		if dfd, derr := syscall.Open(req.DataFile, syscall.O_RDONLY|syscall.O_DIRECT, 0); derr == nil {
			directFD = dfd
			direct = true
		} else {
			log.Printf("O_DIRECT open failed, falling back to buffered read: %v", derr)
		}
	}

	readStart := time.Now()
	var got int
	var readErr error
	path := string(a.mode)
	var cacheHit, cachePartial, cacheMiss, probeErr bool
	var probeMs int64

	if probeCache {
		// preadv2(RWF_NOWAIT) only succeeds without blocking when the range is
		// already in the page cache. A full success is not a wasted probe: the
		// bytes are already in the destination buffer.
		n, perr := unix.Preadv2(fd, [][]byte{buf.slice[:available]}, w.readOffset, unix.RWF_NOWAIT)
		probeMs = time.Since(readStart).Milliseconds()
		switch {
		case perr == nil && int64(n) == available:
			got = n
			path = "cache"
			cacheHit = true
		case perr == nil:
			// Partial hit: discard and re-read the whole window directly.
			path = "direct"
			cachePartial = true
		case errors.Is(perr, unix.EAGAIN):
			path = "direct"
			cacheMiss = true
		default:
			path = "direct"
			probeErr = true
		}
	}

	if path != "cache" {
		if direct {
			got, readErr = readFullyAt(directFD, buf.slice[:directLen], w.readOffset)
			if readErr == nil && int64(got) == directLen && available > directLen {
				// The sub-block tail must go through a buffered descriptor: an
				// O_DIRECT descriptor rejects it with EINVAL.
				var tail int
				tail, readErr = readFullyAt(fd, buf.slice[directLen:available], w.readOffset+directLen)
				got += tail
			}
		} else {
			got, readErr = readFullyAt(fd, buf.slice[:available], w.readOffset)
		}
		if path == "auto" {
			path = "buffered-fallback"
		}
	}
	readMs := time.Since(readStart).Milliseconds()
	if directFD >= 0 {
		syscall.Close(directFD)
	}
	syscall.Close(fd)

	a.mu.Lock()
	a.requests++
	a.last = req
	a.readBy += int64(got)
	if direct {
		a.directHi++
	} else {
		a.directLo++
	}
	if readErr != nil || int64(got) != available {
		a.short++
	}
	if cacheHit {
		a.cacheHit++
	}
	if cachePartial {
		a.cachePart++
	}
	if cacheMiss {
		a.cacheMiss++
	}
	if probeErr {
		a.probeErr++
	}
	a.mu.Unlock()

	if readErr != nil {
		return computeResponse{Error: "read: " + readErr.Error()}, http.StatusInternalServerError
	}
	if int64(got) != available {
		return computeResponse{
			Error: fmt.Sprintf("short read of %s: wanted %d bytes at offset %d, got %d",
				req.DataFile, available, w.readOffset, got),
		}, http.StatusInternalServerError
	}

	kernelStart := time.Now()
	sum := sumPayload(buf.slice, w.payloadShift, req.Size)
	kernelMs := time.Since(kernelStart).Milliseconds()

	log.Printf("compute size=%d window=[%d,%d) shift=%d mode=%s path=%s direct=%v probe_ms=%d read_ms=%d kernel_ms=%d result=%d",
		req.Size, w.readOffset, w.readOffset+w.readSize, w.payloadShift, a.mode, path, direct, probeMs, readMs, kernelMs, sum)

	return computeResponse{
		Result:    fmt.Sprintf("%d", sum),
		ReadMs:    readMs,
		KernelMs:  kernelMs,
		Direct:    direct,
		ReadBytes: int64(got),
		Path:      path,
		ProbeMs:   probeMs,
	}, http.StatusOK
}

func (a *agent) handleStats(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":          string(a.mode),
		"requests":      a.requests,
		"direct_hits":   a.directHi,
		"direct_misses": a.directLo,
		"short_reads":   a.short,
		"read_bytes":    a.readBy,
		"evict":         a.evict,
		"cache_hits":    a.cacheHit,
		"cache_partial": a.cachePart,
		"cache_misses":  a.cacheMiss,
		"probe_errors":  a.probeErr,
	})
}

func (a *agent) handleLast(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	writeJSON(w, http.StatusOK, a.last)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18090", "listen address")
	mode := flag.String("mode", string(modeDirect), "read path: legacy|aligned|direct")
	evict := flag.Bool("evict", false, "drop the data file from the page cache before each read (cold-cache measurement)")
	flag.Parse()

	selected := readMode(*mode)
	switch selected {
	case modeLegacy, modeAligned, modeDirect, modeAuto:
	default:
		fmt.Fprintf(os.Stderr, "unknown -mode %q (want legacy, aligned, direct or auto)\n", *mode)
		os.Exit(2)
	}

	a := &agent{mode: selected, evict: *evict}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/compute", a.handleCompute)
	mux.HandleFunc("/v1/stats", a.handleStats)
	mux.HandleFunc("/v1/last", a.handleLast)

	log.Printf("csd-agent-sim ready on %s mode=%s", *listen, selected)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatalf("csd-agent-sim: %v", err)
	}
}
