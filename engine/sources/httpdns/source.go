package httpdns

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/pkg/idtools"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/executor/oci"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/source"
	srchttp "github.com/moby/buildkit/source/http"
	"github.com/moby/buildkit/util/tracing"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"

	"github.com/dagger/dagger/network"
)

type Opt struct {
	srchttp.Opt
	BaseDNSConfig *oci.DNSConfig
}

type httpSource struct {
	src source.Source

	cache     cache.Accessor
	transport http.RoundTripper
	dns       *oci.DNSConfig
}

func NewSource(opt Opt) (source.Source, error) {
	src, err := srchttp.NewSource(opt.Opt)
	if err != nil {
		return nil, err
	}

	transport := opt.Transport
	if transport == nil {
		transport = tracing.DefaultTransport
	}
	hs := &httpSource{
		src:       src,
		cache:     opt.CacheAccessor,
		transport: transport,
		dns:       opt.BaseDNSConfig,
	}
	return hs, nil
}

func (hs *httpSource) Schemes() []string {
	return hs.src.Schemes()
}

func (hs *httpSource) Identifier(scheme, ref string, attrs map[string]string, platform *pb.Platform) (source.Identifier, error) {
	srcid, err := hs.src.Identifier(scheme, ref, attrs, platform)
	if err != nil {
		return nil, err
	}
	id := &HTTPIdentifier{
		HTTPIdentifier: *(srcid.(*srchttp.HTTPIdentifier)),
	}

	if v, ok := attrs[AttrHTTPClientIDs]; ok {
		id.ClientIDs = strings.Split(v, ",")
	}

	return id, nil
}

func (hs *httpSource) Resolve(ctx context.Context, id source.Identifier, sm *session.Manager, _ solver.Vertex) (source.SourceInstance, error) {
	httpIdentifier, ok := id.(*HTTPIdentifier)
	if !ok {
		return nil, errors.Errorf("invalid http identifier %v", id)
	}

	return &httpSourceHandler{
		httpSource: hs,
		src:        *httpIdentifier,
		sm:         sm,
	}, nil
}

type httpSourceHandler struct {
	*httpSource
	src      HTTPIdentifier
	refID    string
	cacheKey digest.Digest
	sm       *session.Manager
}

func (hs *httpSourceHandler) client(g session.Group) *http.Client {
	clientDomains := []string{}
	for _, clientID := range hs.src.ClientIDs {
		clientDomains = append(clientDomains, network.ClientDomain(clientID))
	}

	dns := *hs.dns
	dns.SearchDomains = append(clientDomains, dns.SearchDomains...)

	return &http.Client{Transport: newTransport(hs.transport, hs.sm, g, &dns)}
}

// urlHash is internal hash the etag is stored by that doesn't leak outside
// this package.
func (hs *httpSourceHandler) urlHash() (digest.Digest, error) {
	dt, err := json.Marshal(struct {
		Filename       string
		Perm, UID, GID int
	}{
		Filename: getFileName(hs.src.URL, hs.src.Filename, nil),
		Perm:     hs.src.Perm,
		UID:      hs.src.UID,
		GID:      hs.src.GID,
	})
	if err != nil {
		return "", err
	}
	return digest.FromBytes(dt), nil
}

func (hs *httpSourceHandler) formatCacheKey(filename string, dgst digest.Digest, lastModTime string) digest.Digest {
	dt, err := json.Marshal(struct {
		Filename       string
		Perm, UID, GID int
		Checksum       digest.Digest
		LastModTime    string `json:",omitempty"`
	}{
		Filename:    filename,
		Perm:        hs.src.Perm,
		UID:         hs.src.UID,
		GID:         hs.src.GID,
		Checksum:    dgst,
		LastModTime: lastModTime,
	})
	if err != nil {
		return dgst
	}
	return digest.FromBytes(dt)
}

func (hs *httpSourceHandler) CacheKey(ctx context.Context, g session.Group, index int) (string, string, solver.CacheOpts, bool, error) {
	if hs.src.Checksum != "" {
		hs.cacheKey = hs.src.Checksum
		return hs.formatCacheKey(getFileName(hs.src.URL, hs.src.Filename, nil), hs.src.Checksum, "").String(), hs.src.Checksum.String(), nil, true, nil
	}

	uh, err := hs.urlHash()
	if err != nil {
		return "", "", nil, false, nil
	}

	// look up metadata(previously stored headers) for that URL
	mds, err := searchHTTPURLDigest(ctx, hs.cache, uh)
	if err != nil {
		return "", "", nil, false, errors.Wrapf(err, "failed to search metadata for %s", uh)
	}

	req, err := http.NewRequest("GET", hs.src.URL, nil)
	if err != nil {
		return "", "", nil, false, err
	}
	req = req.WithContext(ctx)
	m := map[string]cacheRefMetadata{}

	// If we request a single ETag in 'If-None-Match', some servers omit the
	// unambiguous ETag in their response.
	// See: https://github.com/moby/buildkit/issues/905
	var onlyETag string

	if len(mds) > 0 {
		for _, md := range mds {
			// if metaDigest := getMetaDigest(si); metaDigest == hs.formatCacheKey("") {
			if etag := md.getETag(); etag != "" {
				if dgst := md.getHTTPChecksum(); dgst != "" {
					m[etag] = md
				}
			}
			// }
		}
		if len(m) > 0 {
			etags := make([]string, 0, len(m))
			for t := range m {
				etags = append(etags, t)
			}
			req.Header.Add("If-None-Match", strings.Join(etags, ", "))

			if len(etags) == 1 {
				onlyETag = etags[0]
			}
		}
	}

	client := hs.client(g)

	// Some servers seem to have trouble supporting If-None-Match properly even
	// though they return ETag-s. So first, optionally try a HEAD request with
	// manual ETag value comparison.
	if len(m) > 0 {
		req.Method = "HEAD"
		// we need to add accept-encoding header manually because stdlib only adds it to GET requests
		// some servers will return different etags if Accept-Encoding header is different
		req.Header.Add("Accept-Encoding", "gzip")
		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotModified {
				respETag := etagValue(resp.Header.Get("ETag"))

				// If a 304 is returned without an ETag and we had only sent one ETag,
				// the response refers to the ETag we asked about.
				if respETag == "" && onlyETag != "" && resp.StatusCode == http.StatusNotModified {
					respETag = onlyETag
				}
				md, ok := m[respETag]
				if ok {
					hs.refID = md.ID()
					dgst := md.getHTTPChecksum()
					if dgst != "" {
						modTime := md.getHTTPModTime()
						resp.Body.Close()
						return hs.formatCacheKey(getFileName(hs.src.URL, hs.src.Filename, resp), dgst, modTime).String(), dgst.String(), nil, true, nil
					}
				}
			}
			resp.Body.Close()
		}
		req.Method = "GET"
		// Unset explicit Accept-Encoding for GET, otherwise the go http library will not
		// transparently decompress the response body when it is gzipped. It will still add
		// this header implicitly when the request is made though.
		req.Header.Del("Accept-Encoding")
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", nil, false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return "", "", nil, false, errors.Errorf("invalid response status %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotModified {
		respETag := etagValue(resp.Header.Get("ETag"))
		if respETag == "" && onlyETag != "" {
			respETag = onlyETag

			// Set the missing ETag header on the response so that it's available
			// to .save()
			resp.Header.Set("ETag", onlyETag)
		}
		md, ok := m[respETag]
		if !ok {
			return "", "", nil, false, errors.Errorf("invalid not-modified ETag: %v", respETag)
		}
		hs.refID = md.ID()
		dgst := md.getHTTPChecksum()
		if dgst == "" {
			return "", "", nil, false, errors.Errorf("invalid metadata change")
		}
		modTime := md.getHTTPModTime()
		resp.Body.Close()
		return hs.formatCacheKey(getFileName(hs.src.URL, hs.src.Filename, resp), dgst, modTime).String(), dgst.String(), nil, true, nil
	}

	ref, dgst, err := hs.save(ctx, resp, g)
	if err != nil {
		return "", "", nil, false, err
	}
	ref.Release(context.TODO())

	hs.cacheKey = dgst

	return hs.formatCacheKey(getFileName(hs.src.URL, hs.src.Filename, resp), dgst, resp.Header.Get("Last-Modified")).String(), dgst.String(), nil, true, nil
}

func (hs *httpSourceHandler) save(ctx context.Context, resp *http.Response, s session.Group) (ref cache.ImmutableRef, dgst digest.Digest, retErr error) {
	newRef, err := hs.cache.New(ctx, nil, s, cache.CachePolicyRetain, cache.WithDescription(fmt.Sprintf("http url %s", hs.src.URL)))
	if err != nil {
		return nil, "", err
	}

	releaseRef := func() {
		newRef.Release(context.TODO())
	}

	defer func() {
		if retErr != nil && newRef != nil {
			releaseRef()
		}
	}()

	mount, err := newRef.Mount(ctx, false, s)
	if err != nil {
		return nil, "", err
	}

	lm := snapshot.LocalMounter(mount)
	dir, err := lm.Mount()
	if err != nil {
		return nil, "", err
	}

	defer func() {
		if retErr != nil && lm != nil {
			lm.Unmount()
		}
	}()
	perm := 0600
	if hs.src.Perm != 0 {
		perm = hs.src.Perm
	}
	fp := filepath.Join(dir, getFileName(hs.src.URL, hs.src.Filename, resp))

	f, err := os.OpenFile(fp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(perm))
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	h := sha256.New()

	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return nil, "", err
	}

	if err := f.Close(); err != nil {
		return nil, "", err
	}
	f = nil

	uid := hs.src.UID
	gid := hs.src.GID
	if idmap := mount.IdentityMapping(); idmap != nil {
		identity, err := idmap.ToHost(idtools.Identity{
			UID: uid,
			GID: gid,
		})
		if err != nil {
			return nil, "", err
		}
		uid = identity.UID
		gid = identity.GID
	}

	if gid != 0 || uid != 0 {
		if err := os.Chown(fp, uid, gid); err != nil {
			return nil, "", err
		}
	}

	mTime := time.Unix(0, 0)
	lastMod := resp.Header.Get("Last-Modified")
	if lastMod != "" {
		if parsedMTime, err := http.ParseTime(lastMod); err == nil {
			mTime = parsedMTime
		}
	}

	if err := os.Chtimes(fp, mTime, mTime); err != nil {
		return nil, "", err
	}

	lm.Unmount()
	lm = nil

	ref, err = newRef.Commit(ctx)
	if err != nil {
		return nil, "", err
	}
	newRef = nil
	md := cacheRefMetadata{ref}

	hs.refID = ref.ID()
	dgst = digest.NewDigest(digest.SHA256, h)

	if respETag := resp.Header.Get("ETag"); respETag != "" {
		respETag = etagValue(respETag)
		if err := md.setETag(respETag); err != nil {
			return nil, "", err
		}
		uh, err := hs.urlHash()
		if err != nil {
			return nil, "", err
		}
		if err := md.setHTTPChecksum(uh, dgst); err != nil {
			return nil, "", err
		}
	}

	if modTime := resp.Header.Get("Last-Modified"); modTime != "" {
		if err := md.setHTTPModTime(modTime); err != nil {
			return nil, "", err
		}
	}

	return ref, dgst, nil
}

func (hs *httpSourceHandler) Snapshot(ctx context.Context, g session.Group) (cache.ImmutableRef, error) {
	if hs.refID != "" {
		ref, err := hs.cache.Get(ctx, hs.refID, nil)
		if err == nil {
			return ref, nil
		}
	}

	req, err := http.NewRequest("GET", hs.src.URL, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)

	client := hs.client(g)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	ref, dgst, err := hs.save(ctx, resp, g)
	if err != nil {
		return nil, err
	}
	if dgst != hs.cacheKey {
		ref.Release(context.TODO())
		return nil, errors.Errorf("digest mismatch %s: %s", dgst, hs.cacheKey)
	}

	return ref, nil
}

func getFileName(urlStr, manualFilename string, resp *http.Response) string {
	if manualFilename != "" {
		return manualFilename
	}
	if resp != nil {
		if contentDisposition := resp.Header.Get("Content-Disposition"); contentDisposition != "" {
			if _, params, err := mime.ParseMediaType(contentDisposition); err == nil {
				if params["filename"] != "" && !strings.HasSuffix(params["filename"], "/") {
					if filename := filepath.Base(filepath.FromSlash(params["filename"])); filename != "" {
						return filename
					}
				}
			}
		}
	}
	u, err := url.Parse(urlStr)
	if err == nil {
		if base := path.Base(u.Path); base != "." && base != "/" {
			return base
		}
	}
	return "download"
}

func searchHTTPURLDigest(ctx context.Context, store cache.MetadataStore, dgst digest.Digest) ([]cacheRefMetadata, error) {
	mds, err := store.Search(ctx, string(dgst))
	if err != nil {
		return nil, err
	}
	results := make([]cacheRefMetadata, len(mds))
	for i, md := range mds {
		results[i] = cacheRefMetadata{md}
	}
	return results, nil
}

type cacheRefMetadata struct {
	cache.RefMetadata
}

const keyHTTPChecksum = "http.checksum"
const keyETag = "etag"
const keyModTime = "http.modtime"

func (md cacheRefMetadata) getHTTPChecksum() digest.Digest {
	return digest.Digest(md.GetString(keyHTTPChecksum))
}

func (md cacheRefMetadata) setHTTPChecksum(urlDgst digest.Digest, d digest.Digest) error {
	return md.SetString(keyHTTPChecksum, d.String(), urlDgst.String())
}

func (md cacheRefMetadata) getETag() string {
	return md.GetString(keyETag)
}

func (md cacheRefMetadata) setETag(s string) error {
	return md.SetString(keyETag, s, "")
}

func (md cacheRefMetadata) getHTTPModTime() string {
	return md.GetString(keyModTime)
}

func (md cacheRefMetadata) setHTTPModTime(s string) error {
	return md.SetString(keyModTime, s, "")
}

func etagValue(v string) string {
	// remove weak for direct comparison
	return strings.TrimPrefix(v, "W/")
}
