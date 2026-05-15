package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acoshift/pgsql/pgctx"
	"gocloud.dev/blob/memblob"
)

func TestGCHandler_Unauthorized(t *testing.T) {
	t.Parallel()
	bkt := memblob.OpenBucket(nil)
	t.Cleanup(func() { bkt.Close() })
	app := &App{Bucket: bkt, InternalSecret: "secret-key"}
	r := httptest.NewRequest(http.MethodPost, "/internal/gc", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	app.gcHandler(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestGCHandler_NoAuthHeader(t *testing.T) {
	t.Parallel()
	bkt := memblob.OpenBucket(nil)
	t.Cleanup(func() { bkt.Close() })
	app := &App{Bucket: bkt, InternalSecret: "secret-key"}
	r := httptest.NewRequest(http.MethodPost, "/internal/gc", nil)
	w := httptest.NewRecorder()
	app.gcHandler(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestGCHandler_AuthorizedSuccess(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	app := &App{Bucket: newTestBucket(t), InternalSecret: "topsecret"}
	r := httptest.NewRequest(http.MethodPost, "/internal/gc", nil)
	r = r.WithContext(db.Ctx())
	r.Header.Set("Authorization", "Bearer topsecret")
	w := httptest.NewRecorder()
	app.gcHandler(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestGCHandler_NoSecretAllowsAnyone(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	// When InternalSecret is empty, the handler does not check Authorization.
	app := &App{Bucket: newTestBucket(t)}
	r := httptest.NewRequest(http.MethodPost, "/internal/gc", nil)
	r = r.WithContext(db.Ctx())
	w := httptest.NewRecorder()
	app.gcHandler(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (no secret required)", w.Code)
	}
}

func TestGCHandler_RouteIntegration(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	app := newTestApp(newTestBucket(t), authorized)
	r := httptest.NewRequest(http.MethodPost, "/internal/gc", nil)
	r = r.WithContext(db.Ctx())
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestRunGC_DeletesExpiredFiles(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}
	ctx := db.Ctx()

	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, created_at, expires_at)
		VALUES ('expired', 'proj-1', 100, 'test.txt', 1, now() - interval '2 days', now() - interval '1 day')
	`); err != nil {
		t.Fatal(err)
	}

	bw, err := bkt.NewWriter(ctx, "expired", nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("data"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := app.runGC(ctx); err != nil {
		t.Fatal(err)
	}

	if n := db.CountFiles(t); n != 0 {
		t.Errorf("db files = %d, want 0 after GC", n)
	}
	if n := countObjects(t, bkt); n != 0 {
		t.Errorf("bucket objects = %d, want 0 after GC", n)
	}
}

func TestRunGC_KeepsNonExpiredFiles(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}
	ctx := db.Ctx()

	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, created_at, expires_at)
		VALUES ('fresh', 'proj-1', 100, 'test.txt', 1, now(), now() + interval '1 day')
	`); err != nil {
		t.Fatal(err)
	}

	bw, err := bkt.NewWriter(ctx, "fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("data"))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := app.runGC(ctx); err != nil {
		t.Fatal(err)
	}

	if n := db.CountFiles(t); n != 1 {
		t.Errorf("db files = %d, want 1 (non-expired file kept)", n)
	}
	if n := countObjects(t, bkt); n != 1 {
		t.Errorf("bucket objects = %d, want 1 (non-expired object kept)", n)
	}
}

func TestRunGC_AlreadyDeletedFromStorage(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}
	ctx := db.Ctx()

	// Insert an expired file but don't put it in the bucket (already deleted from storage).
	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, created_at, expires_at)
		VALUES ('orphan', 'proj-1', 100, 'test.txt', 1, now() - interval '2 days', now() - interval '1 day')
	`); err != nil {
		t.Fatal(err)
	}

	// GC should ignore the storage 404 and still clean up the DB row.
	if err := app.runGC(ctx); err != nil {
		t.Fatal(err)
	}

	if n := db.CountFiles(t); n != 0 {
		t.Errorf("db files = %d, want 0 (orphan DB row cleaned up)", n)
	}
}

func TestRunGC_MixedExpiry(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}
	ctx := db.Ctx()

	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, created_at, expires_at) VALUES
			('expired', 'proj-1', 100, 'a.txt', 1, now() - interval '2 days', now() - interval '1 day'),
			('fresh',   'proj-1', 100, 'b.txt', 1, now(),                     now() + interval '1 day')
	`); err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{"expired", "fresh"} {
		bw, err := bkt.NewWriter(ctx, fn, nil)
		if err != nil {
			t.Fatal(err)
		}
		bw.Write([]byte("data"))
		if err := bw.Close(); err != nil {
			t.Fatal(err)
		}
	}

	if err := app.runGC(ctx); err != nil {
		t.Fatal(err)
	}

	if n := db.CountFiles(t); n != 1 {
		t.Errorf("db files = %d, want 1 (only non-expired kept)", n)
	}
	if n := countObjects(t, bkt); n != 1 {
		t.Errorf("bucket objects = %d, want 1 (only non-expired kept)", n)
	}
}

func TestRunGC_NoExpiredFiles(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}

	if err := app.runGC(db.Ctx()); err != nil {
		t.Fatal(err)
	}
}
