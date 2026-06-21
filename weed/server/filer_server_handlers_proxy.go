package weed_server

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/glog"
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

func (fs *FilerServer) proxyComputeToVolumeServer(w http.ResponseWriter, r *http.Request, entry *filer.Entry, operation string) {
	ctx := r.Context()
	fileId, err := computeFileIdFromEntry(ctx, fs, entry)
	if err != nil {
		writeJsonError(w, r, http.StatusBadRequest, err)
		return
	}
	urlStrings, err := fs.filer.MasterClient.GetLookupFileIdFunction()(ctx, fileId)
	if err != nil {
		glog.ErrorfCtx(ctx, "locate compute target %s: %v", fileId, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if len(urlStrings) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	target, err := url.Parse(urlStrings[rand.IntN(len(urlStrings))])
	if err != nil {
		writeJsonError(w, r, http.StatusInternalServerError, err)
		return
	}
	if entry.Name() != "" {
		target.Path = volumeComputePath(fileId, entry.Name())
	}
	query := r.URL.Query()
	query.Set(volumeComputeQuery, operation)
	target.RawQuery = query.Encode()

	proxyReq, err := http.NewRequestWithContext(ctx, r.Method, target.String(), nil)
	if err != nil {
		writeJsonError(w, r, http.StatusInternalServerError, err)
		return
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
		glog.V(0).InfofCtx(ctx, "compute proxy to %s cancelled while waiting: %v", volumeHost, err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	defer releaseProxySemaphore(volumeHost)

	proxyResponse, postErr := util_http.GetGlobalHttpClient().Do(proxyReq)
	if postErr != nil {
		glog.ErrorfCtx(ctx, "compute proxy to volume: %v", postErr)
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
		glog.V(0).InfofCtx(ctx, "compute proxy copy %s: %v", fileId, copyErr)
	}
}

func computeFileIdFromEntry(ctx context.Context, fs *FilerServer, entry *filer.Entry) (string, error) {
	if len(entry.Content) > 0 {
		return "", fmt.Errorf("compute is only supported for volume-backed files")
	}
	chunks := entry.GetChunks()
	if len(chunks) == 0 {
		return "", fmt.Errorf("compute is only supported for volume-backed files")
	}
	dataChunks, _, err := filer.ResolveChunkManifest(ctx, fs.filer.MasterClient.GetLookupFileIdFunction(), chunks, 0, int64(entry.FileSize))
	if err != nil {
		return "", fmt.Errorf("resolve chunk manifest: %w", err)
	}
	if len(dataChunks) != 1 {
		return "", fmt.Errorf("compute currently supports single-chunk files only, got %d chunks", len(dataChunks))
	}
	chunk := dataChunks[0]
	if chunk.GetOffset() != 0 || chunk.GetSize() != entry.FileSize {
		return "", fmt.Errorf("compute currently requires one chunk covering the whole file")
	}
	if len(chunk.GetCipherKey()) > 0 || chunk.GetSseType() != 0 {
		return "", fmt.Errorf("compute is not supported for encrypted chunks")
	}
	fileId := chunk.GetFileIdString()
	if fileId == "" {
		return "", fmt.Errorf("compute target chunk has empty file id")
	}
	return fileId, nil
}

func volumeComputePath(fileId string, filename string) string {
	vid, fid, found := strings.Cut(fileId, ",")
	if !found {
		return "/" + fileId
	}
	return "/" + vid + "/" + fid + "/" + url.PathEscape(filename)
}
