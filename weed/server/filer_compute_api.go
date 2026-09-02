package weed_server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// Compute API base path.
//
// The filer already exposes compute for the file interface through
//
//	GET /<path>?compute=<operation>
//
// This handler is the upper-layer entry point that lets the same
// multi-chunk compute engine be addressed through three storage
// interface styles:
//
//	GET /api/compute/file/<filer-path>?compute=<operation>
//	GET /api/compute/object/<bucket>/<object-key>?compute=<operation>
//	GET /api/compute/block/<volume>?compute=<operation>
//
// object maps <bucket>/<key> into <DirBucketsPath>/<bucket>/<key>, i.e. the
// same namespace S3 objects live in. block treats a raw block image stored at
// /blocks/<volume> as a logical block volume and runs the operator over the
// whole backing image. The real work is always delegated to the existing
// proxyComputeToVolumeServer orchestration (chunk fan-out + volume-local
// compute + filer aggregation).
const computeAPIBasePath = "/api/compute/"

type computeInterface string

const (
	computeInterfaceFile   computeInterface = "file"
	computeInterfaceObject computeInterface = "object"
	computeInterfaceBlock  computeInterface = "block"
)

// computeAPITarget resolves a multi-interface compute request into the filer
// path of the data to compute on.
func (fs *FilerServer) computeAPITarget(iface computeInterface, resource string, query url.Values) (string, error) {
	resource = strings.Trim(resource, "/")
	if resource == "" {
		return "", errors.New("compute resource is empty")
	}
	switch iface {
	case computeInterfaceFile:
		return "/" + resource, nil
	case computeInterfaceObject:
		bucket, rest, found := strings.Cut(resource, "/")
		if !found || bucket == "" || rest == "" {
			return "", errors.New("object compute requires /api/compute/object/<bucket>/<object-key>")
		}
		return strings.TrimRight(fs.filer.DirBucketsPath, "/") + "/" + bucket + "/" + rest, nil
	case computeInterfaceBlock:
		// A block volume may optionally be backed by an explicit filer path.
		// Without it, the volume name itself is the filer path of the raw
		// block image (e.g. /blocks/vol0), matching the convention used by the
		// block-compute client.
		if backing := query.Get("path"); backing != "" {
			return backing, nil
		}
		return "/blocks/" + resource, nil
	default:
		return "", errors.New("unknown compute interface")
	}
}

// multiProtocolComputeHandler is registered at /api/compute/ on the filer HTTP
// server. All three storage interface styles converge on it and from here on
// share the exact same filer->volume compute orchestration.
func (fs *FilerServer) multiProtocolComputeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJsonError(w, r, http.StatusMethodNotAllowed, errors.New("compute API only supports GET"))
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, computeAPIBasePath)
	ifaceName, resource, found := strings.Cut(rel, "/")
	if !found {
		ifaceName = rel
		resource = ""
	}
	iface := computeInterface(ifaceName)
	query := r.URL.Query()
	operation := query.Get(volumeComputeQuery)
	if operation == "" {
		writeJsonError(w, r, http.StatusBadRequest, errors.New("missing compute operation, use ?compute=<operation>"))
		return
	}

	target, err := fs.computeAPITarget(iface, resource, query)
	if err != nil {
		writeJsonError(w, r, http.StatusBadRequest, err)
		return
	}

	entry, err := fs.filer.FindEntry(r.Context(), util.FullPath(target))
	if err != nil {
		if err == filer_pb.ErrNotFound {
			writeJsonError(w, r, http.StatusNotFound, errors.New("compute target not found"))
			return
		}
		writeJsonError(w, r, http.StatusInternalServerError, err)
		return
	}
	if entry.IsDirectory() {
		writeJsonError(w, r, http.StatusBadRequest, errors.New("compute target is a directory"))
		return
	}

	glog.V(0).Infof("compute API %q %q -> %s (operation %q)", iface, resource, target, operation)
	// The actual computation is the existing single/multi-chunk engine. It only
	// inspects the request query and headers, not r.URL.Path, so it can be
	// reused unchanged by every compute API namespace.
	fs.proxyComputeToVolumeServer(w, r, entry, operation)
}
