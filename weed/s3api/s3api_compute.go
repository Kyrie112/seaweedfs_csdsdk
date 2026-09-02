package s3api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3_constants"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3err"
	"github.com/seaweedfs/seaweedfs/weed/util"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
)

const (
	// s3ComputeHeader and s3ComputeQuery let an object-storage client trigger
	// the same compute engine as the file interface:
	//
	//   GET /bucket/object?x-compute=rawsum64
	//   GET /bucket/object  (with X-SeaweedFS-Compute: rawsum64)
	//
	// The S3 gateway is only a thin protocol adapter: it resolves bucket/object
	// to the underlying filer path and forwards the compute query to the same
	// filer-side engine. Chunk fan-out and aggregation happen on filer/volume.
	s3ComputeQuery  = "x-compute"
	s3ComputeHeader = "X-SeaweedFS-Compute"
)

// handleObjectCompute returns true when the request was a compute request and
// has been fully handled. It is called after S3 read authorization has already
// passed, so compute inherits the bucket/object read policy.
func (s3a *S3ApiServer) handleObjectCompute(w http.ResponseWriter, r *http.Request, bucket, object string) bool {
	operation := r.URL.Query().Get(s3ComputeQuery)
	if operation == "" {
		operation = r.Header.Get(s3ComputeHeader)
	}
	if operation == "" {
		return false
	}

	filerPath := string(util.NewFullPath(s3a.bucketDir(bucket), s3_constants.NormalizeObjectKey(object)))
	if err := s3a.forwardComputeToFiler(w, r, filerPath, operation); err != nil {
		glog.Errorf("S3 compute %s/%s -> %s (%s) failed: %v", bucket, object, filerPath, operation, err)
		s3err.WriteErrorResponse(w, r, s3err.ErrInternalError)
	}
	return true
}

// forwardComputeToFiler issues the actual compute request against the filer.
// Keeping this as an HTTP forward makes the S3 gateway independent of filer
// process internals and reuses the same filer->volume engine for every storage
// interface. In deployments with filer HTTP auth enabled, the same signing
// credentials used by the S3 gateway must be accepted by the filer endpoint.
func (s3a *S3ApiServer) forwardComputeToFiler(w http.ResponseWriter, r *http.Request, filerPath string, operation string) error {
	var filerAddr string
	for _, f := range s3a.option.Filers {
		if addr := f.ToHttpAddress(); addr != "" {
			filerAddr = addr
			break
		}
	}
	if filerAddr == "" {
		return errors.New("no filer http address configured for S3 compute")
	}

	u := &url.URL{
		Scheme: "http",
		Host:   filerAddr,
		Path:   filerPath,
	}
	q := u.Query()
	q.Set("compute", operation)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := util_http.GetGlobalHttpClient().Do(req)
	if err != nil {
		return err
	}
	defer util_http.CloseResponse(resp)

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		return fmt.Errorf("stream compute response: %w", err)
	}
	return nil
}
