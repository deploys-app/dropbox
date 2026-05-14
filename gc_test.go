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
	app := &App{Bucket: newTestBucket(t), InternalSecret: "topsecret"}
	r := httptest.NewRequest(http.MethodPost, "/internal/gc", nil)
	r = r.WithContext(testDB.Ctx())
	r.Header.Set("Authorization", "Bearer topsecret")
	w := httptest.NewRecorder()
	app.gcHandler(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestGCHandler_NoSecretAllowsAnyone(t *testing.T) {
	t.Parallel()
	// When InternalSecret is empty, the handler does not check Authorization.
	app := &App{Bucket: newTestBucket(t)}
	r := httptest.NewRequest(http.MethodPost, "/internal/gc", nil)
	r = r.WithContext(testDB.Ctx())
	w := httptest.NewRecorder()
	app.gcHandler(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (no secret required)", w.Code)
	}
}

func TestGCHandler_RouteIntegration(t *testing.T) {
	t.Parallel()
	app := newTestApp(newTestBucket(t), authorized)
	r := httptest.NewRequest(http.MethodPost, "/internal/gc", nil)
	r = r.WithContext(testDB.Ctx())
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestRunGC_DeletesExpiredFiles(t *testing.T) {
	t.Parallel()
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}
	ctx := testDB.Ctx()
	projectID := "proj-" + t.Name()
	fn := "exp-" + t.Name()

	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, created_at, expires_at)
		VALUES ($1, $2, 100, 'test.txt', 1, now() - interval '2 days', now() - interval '1 day')
	`, fn, projectID); err != nil {
		t.Fatal(err)
	}

	bw, err := bkt.NewWriter(ctx, fn, nil)
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

	if n := testDB.CountFilesByProject(t, projectID); n != 0 {
		t.Errorf("db files for %q = %d, want 0 after GC", projectID, n)
	}
	if n := countObjects(t, bkt); n != 0 {
		t.Errorf("bucket objects = %d, want 0 after GC", n)
	}
}

func TestRunGC_KeepsNonExpiredFiles(t *testing.T) {
	t.Parallel()
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}
	ctx := testDB.Ctx()
	projectID := "proj-" + t.Name()
	fn := "fresh-" + t.Name()

	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, created_at, expires_at)
		VALUES ($1, $2, 100, 'test.txt', 1, now(), now() + interval '1 day')
	`, fn, projectID); err != nil {
		t.Fatal(err)
	}

	bw, err := bkt.NewWriter(ctx, fn, nil)
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

	if n := testDB.CountFilesByProject(t, projectID); n != 1 {
		t.Errorf("db files for %q = %d, want 1 (non-expired file kept)", projectID, n)
	}
	if n := countObjects(t, bkt); n != 1 {
		t.Errorf("bucket objects = %d, want 1 (non-expired object kept)", n)
	}
}

func TestRunGC_AlreadyDeletedFromStorage(t *testing.T) {
	t.Parallel()
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}
	ctx := testDB.Ctx()
	projectID := "proj-" + t.Name()
	fn := "orphan-" + t.Name()

	// Insert an expired file but don't put it in the bucket (already deleted from storage).
	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, created_at, expires_at)
		VALUES ($1, $2, 100, 'test.txt', 1, now() - interval '2 days', now() - interval '1 day')
	`, fn, projectID); err != nil {
		t.Fatal(err)
	}

	// GC should ignore the storage 404 and still clean up the DB row.
	if err := app.runGC(ctx); err != nil {
		t.Fatal(err)
	}

	if n := testDB.CountFilesByProject(t, projectID); n != 0 {
		t.Errorf("db files for %q = %d, want 0 (orphan DB row cleaned up)", projectID, n)
	}
}

func TestRunGC_MixedExpiry(t *testing.T) {
	t.Parallel()
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}
	ctx := testDB.Ctx()
	projectID := "proj-" + t.Name()
	expFn := "exp-" + t.Name()
	freshFn := "fresh-" + t.Name()

	if _, err := pgctx.Exec(ctx, `
		INSERT INTO files (fn, project_id, size, filename, ttl, created_at, expires_at) VALUES
			($1, $3, 100, 'a.txt', 1, now() - interval '2 days', now() - interval '1 day'),
			($2, $3, 100, 'b.txt', 1, now(),                     now() + interval '1 day')
	`, expFn, freshFn, projectID); err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{expFn, freshFn} {
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

	if n := testDB.CountFilesByProject(t, projectID); n != 1 {
		t.Errorf("db files for %q = %d, want 1 (only non-expired kept)", projectID, n)
	}
	if n := countObjects(t, bkt); n != 1 {
		t.Errorf("bucket objects = %d, want 1 (only non-expired kept)", n)
	}
}

func TestRunGC_NoExpiredFiles(t *testing.T) {
	t.Parallel()
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}
	ctx := testDB.Ctx()

	if err := app.runGC(ctx); err != nil {
		t.Fatal(err)
	}
}
