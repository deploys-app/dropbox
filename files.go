package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoshift/pgsql/pgctx"
	"github.com/moonrhythm/cachestore"
	"github.com/moonrhythm/sf"
	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"
)

const fileMetaCacheTTL = 60 * time.Second

// fileMeta is the file's row in the files table plus a couple of derived
// negative-cache flags. Found is false when there is no row for the fn —
// that happens when the metadata insert failed during upload; we still
// serve the bucket bytes in that case rather than 404'ing, since we have
// no expiration to enforce against. BucketMissing is set by the file
// handlers (not by lookupFile) after a confirmed Bucket.Attributes
// NotFound, so subsequent requests within the cache TTL can return 404
// without re-hitting GCS — that's what stops a DDoS against a dead-but-
// valid-format fn from amplifying into a GCS read flood.
type fileMeta struct {
	ProjectID     string
	ExpiresAt     time.Time
	Found         bool
	BucketMissing bool
}

// Expired reports whether the file has a recorded expiry that is now in
// the past. Files without a DB row (Found == false) and files with a NULL
// expires_at are treated as non-expired — we have no basis to refuse them.
func (m fileMeta) Expired() bool {
	return m.Found && !m.ExpiresAt.IsZero() && m.ExpiresAt.Before(time.Now())
}

func fileCacheKey(fn string) string { return "fn|" + fn }

func (a *App) fileHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")

	// Verify the HMAC tag in the token before any I/O. A flood of random
	// /files/{garbage} requests by an attacker who doesn't have SignKey
	// is rejected in CPU here — no DB query, no GCS Attributes call.
	// This is the primary DDoS shield; the per-fn cache and the
	// negative bucket cache only kick in once we've established the
	// token is one we issued.
	fn, ok := parseToken(a.SignKey, token)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// Check the DB *before* touching GCS so a DDoS against an expired fn
	// is absorbed by the in-process cache (60s TTL on an immutable row)
	// instead of becoming one billed Bucket.Attributes call per request.
	// For non-expired files this just reorders two calls we'd make anyway;
	// for files with no DB row (insert-failed uploads) we fall through to
	// the bucket exactly as before.
	meta := lookupFile(r.Context(), fn)
	if meta.Expired() {
		http.Error(w, "file expired", http.StatusGone)
		return
	}
	if meta.BucketMissing {
		// Cached negative from a previous bucket NotFound — see the
		// Attributes branch below.
		http.NotFound(w, r)
		return
	}

	attrs, err := a.Bucket.Attributes(r.Context(), fn)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			// Cache the negative so a flood against the same dead fn
			// doesn't re-bill a Class B op per request. Safe even in the
			// rare orphan case (Found=true, bucket gone): the file is
			// unreachable either way until an operator cleans it up.
			meta.BucketMissing = true
			cachestore.Set(fileCacheKey(fn), meta, &cachestore.SetOptions{TTL: fileMetaCacheTTL})
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// When a CDN is configured and the caller is not in-cluster, record the
	// download as if we were serving the full file (the CDN will handle the
	// actual bytes) and redirect. The redirect target preserves the full
	// signed token so the CDN's origin-fetch hits cdnFileHandler with a
	// URL we can re-verify. Cache hits never come back to this origin, so
	// the bill is an over-approximation; that matches the registry pattern
	// and is what /_internal/calculate-dropbox-usages assumes.
	if a.CDNBaseURL != "" && !isInternalClient(r) {
		downloadCount.WithLabelValues(meta.ProjectID).Inc()
		egressBytes.WithLabelValues(meta.ProjectID).Add(float64(attrs.Size))
		http.Redirect(w, r, a.CDNBaseURL+token, http.StatusTemporaryRedirect)
		return
	}

	a.streamFile(w, r, fn, attrs, meta.ProjectID)
}

// cdnDeadResponseTTL is how long the CDN edge should cache "this URL is
// dead" responses (410 expired, 404 bucket-missing). Long enough that a
// shared-then-expired URL stops hammering origin, short enough that the
// answer can update if we ever change behavior.
const cdnDeadResponseTTL = time.Hour

// cdnFileHandler is the origin endpoint the CDN edge fetches from. The
// token in the URL is the same signed token we issued to the user, so
// the HMAC check still gates the edge — only URLs we issued can reach
// the bucket. Streams the body without metrics so the CDN sees a plain
// response; expired files are refused here too so the CDN can't refresh
// its cache with bytes the user is no longer entitled to.
//
// Each response sets Cache-Control to tell the edge how long to cache:
//   - success (200): public, max-age={remaining TTL}, immutable — fn is
//     unique per upload so the body never changes; cap at the file's
//     expires_at so the edge stops serving past end-of-life.
//   - expired (410) and bucket-missing (404): public, max-age=3600 —
//     the answer is permanent, so let the edge absorb repeat probes.
//   - invalid token (404): no Cache-Control. Every garbage token is a
//     unique URL, so caching doesn't reduce origin load and would just
//     burn edge cache slots on attacker traffic.
func (a *App) cdnFileHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")

	// Same DDoS-protection ladder as fileHandler: verify the signature,
	// then consult the per-fn cache, then GCS. The edge would otherwise
	// amplify a random-garbage flood or an expired-URL probe into one
	// GCS op per request.
	fn, ok := parseToken(a.SignKey, token)
	if !ok {
		http.NotFound(w, r)
		return
	}

	meta := lookupFile(r.Context(), fn)
	if meta.Expired() {
		setDeadCacheControl(w)
		http.Error(w, "file expired", http.StatusGone)
		return
	}
	if meta.BucketMissing {
		setDeadCacheControl(w)
		http.NotFound(w, r)
		return
	}

	attrs, err := a.Bucket.Attributes(r.Context(), fn)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			meta.BucketMissing = true
			cachestore.Set(fileCacheKey(fn), meta, &cachestore.SetOptions{TTL: fileMetaCacheTTL})
			setDeadCacheControl(w)
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Pre-set Cache-Control so streamFile's attrs.CacheControl fallback
	// doesn't overwrite it. If we have no expires_at to work from
	// (insert-failed upload), fall through to the bucket's value.
	if !meta.ExpiresAt.IsZero() {
		remaining := max(time.Until(meta.ExpiresAt), 0)
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d, immutable", int(remaining.Seconds())))
	}

	a.streamFile(w, r, fn, attrs, "")
}

func setDeadCacheControl(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(cdnDeadResponseTTL.Seconds())))
}

// streamFile writes the object body to w, honoring HTTP Range requests.
// The object is handed to http.ServeContent as a lazy io.ReadSeeker
// (blobReadSeeker) so a range request reads only the requested span from
// the bucket instead of the whole object; ServeContent emits 206 +
// Content-Range + Accept-Ranges (or 416 on an unsatisfiable range) and the
// matching Content-Length, and also handles conditional (If-Range /
// If-Modified-Since) requests against the object's mod time. When projectID
// is non-empty, download_count is bumped once per request and egress_bytes
// by the bytes actually written to the client (a range serves fewer than
// attrs.Size). Used by both the direct-stream path (no CDN configured) and
// the unauthenticated _cdn origin path.
func (a *App) streamFile(w http.ResponseWriter, r *http.Request, fn string, attrs *blob.Attributes, projectID string) {
	// Only fall back to the bucket's CacheControl if the caller hasn't
	// already chosen one. cdnFileHandler pre-sets a TTL-aligned policy;
	// the non-CDN fileHandler leaves it unset and gets the bucket value.
	if attrs.CacheControl != "" && w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", attrs.CacheControl)
	}
	if attrs.ContentDisposition != "" {
		w.Header().Set("Content-Disposition", attrs.ContentDisposition)
	}
	// Set Content-Type up front so ServeContent uses it verbatim instead of
	// sniffing (a sniff would cost an extra range read at offset 0). With no
	// stored type, ServeContent falls back to sniffing the first 512 bytes.
	if attrs.ContentType != "" {
		w.Header().Set("Content-Type", attrs.ContentType)
	}

	rs := &blobReadSeeker{ctx: r.Context(), bucket: a.Bucket, key: fn, size: attrs.Size}
	defer rs.Close()

	dst := w
	var counter *countingResponseWriter
	if projectID != "" {
		downloadCount.WithLabelValues(projectID).Inc()
		counter = &countingResponseWriter{ResponseWriter: w}
		dst = counter
	}

	// ServeContent parses Range/If-Range and conditional headers, then
	// streams via rs (which opens a GCS range reader lazily at the requested
	// offset). name is "" — fn carries no meaningful extension and the
	// Content-Type is already chosen above.
	http.ServeContent(dst, r, "", attrs.ModTime, rs)

	if counter != nil {
		egressBytes.WithLabelValues(projectID).Add(float64(counter.n))
	}
}

// blobReadSeeker adapts a GCS object into an io.ReadSeeker backed by lazy
// range reads, so http.ServeContent can satisfy a Range request by reading
// only the requested span instead of the whole object. Seeks are pure
// arithmetic against the known size; the underlying bucket reader is opened
// (and reopened after a discontiguous seek) lazily on the next Read. Not
// safe for concurrent use — one per request, which is how ServeContent
// drives it.
type blobReadSeeker struct {
	ctx    context.Context
	bucket *blob.Bucket
	key    string
	size   int64

	offset int64         // virtual cursor: next byte the caller will read
	rc     io.ReadCloser // current bucket reader, nil until first Read
	rcAt   int64         // absolute offset of rc's next byte; valid when rc != nil
}

func (b *blobReadSeeker) Read(p []byte) (int, error) {
	if b.offset >= b.size {
		return 0, io.EOF
	}
	// (Re)open the bucket reader when we have none, or the cursor has moved
	// away from where the open reader is positioned (after a Seek).
	if b.rc == nil || b.rcAt != b.offset {
		if b.rc != nil {
			b.rc.Close()
			b.rc = nil
		}
		rc, err := b.bucket.NewRangeReader(b.ctx, b.key, b.offset, -1, nil)
		if err != nil {
			return 0, err
		}
		b.rc = rc
		b.rcAt = b.offset
	}
	n, err := b.rc.Read(p)
	b.offset += int64(n)
	b.rcAt += int64(n)
	return n, err
}

func (b *blobReadSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = b.offset + offset
	case io.SeekEnd:
		abs = b.size + offset
	default:
		return 0, errors.New("blobReadSeeker: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("blobReadSeeker: negative position")
	}
	b.offset = abs
	return abs, nil
}

func (b *blobReadSeeker) Close() error {
	if b.rc == nil {
		return nil
	}
	err := b.rc.Close()
	b.rc = nil
	return err
}

// countingResponseWriter tallies bytes written to the response body so
// streamFile can bill egress for the bytes actually sent — a Range response
// transfers fewer than attrs.Size.
type countingResponseWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

// lookupFile fetches the file's project owner and expiration from the DB.
// Both fields are immutable once written, so the in-process cache only
// needs to be short enough to absorb bursts on the same fn — not to
// track any state that can change underneath us.
//
// The DB query runs inside sf.Do so a thundering herd at the cache-miss
// edge (cold start, or the moment a 60s entry expires under load)
// collapses to a single Postgres round-trip. Every concurrent caller
// for the same fn gets the same result without each one issuing its
// own query.
func lookupFile(ctx context.Context, fn string) fileMeta {
	cacheKey := fileCacheKey(fn)
	if v, ok := cachestore.Get[fileMeta](cacheKey); ok {
		return v
	}

	m, _, _ := sf.Do(ctx, "lookupFile|"+fn, func(ctx context.Context) (fileMeta, error) {
		// Re-check the cache: a sibling caller may have populated it
		// while we were queued behind sf's mutex.
		if v, ok := cachestore.Get[fileMeta](cacheKey); ok {
			return v, nil
		}

		recordLookupFileDBQuery(fn)
		var (
			m       fileMeta
			expires sql.NullTime
		)
		err := pgctx.QueryRow(ctx, `
			SELECT project_id, expires_at FROM files WHERE fn = $1
		`, fn).Scan(&m.ProjectID, &expires)
		switch {
		case err == nil:
			m.Found = true
			if expires.Valid {
				m.ExpiresAt = expires.Time
			}
		case errors.Is(err, sql.ErrNoRows):
			// Found=false — a confirmed miss is fine to cache.
		default:
			// Transient DB error: don't cache-poison. Returning here
			// also skips the cache write below; the next caller will
			// retry the query.
			slog.Error("lookup file", "fn", fn, "error", err)
			return fileMeta{}, nil
		}

		cachestore.Set(cacheKey, m, &cachestore.SetOptions{TTL: fileMetaCacheTTL})
		return m, nil
	})
	return m
}

// lookupFileDBQueries counts the number of actual Postgres SELECTs
// issued by lookupFile, keyed by fn. The counter is only bumped inside
// the singleflight closure, so a successful sf.Do collapse leaves it at
// 1 even after N concurrent callers. Production code only writes here;
// only tests read it. Per-fn so parallel tests don't trample each other.
var lookupFileDBQueries sync.Map // map[string]*atomic.Uint64

func recordLookupFileDBQuery(fn string) {
	v, _ := lookupFileDBQueries.LoadOrStore(fn, new(atomic.Uint64))
	v.(*atomic.Uint64).Add(1)
}

// lookupFileDBQueryCount returns how many DB queries lookupFile has
// issued for fn since process start. Test-only.
func lookupFileDBQueryCount(fn string) uint64 {
	v, ok := lookupFileDBQueries.Load(fn)
	if !ok {
		return 0
	}
	return v.(*atomic.Uint64).Load()
}
