package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acoshift/pgsql/pgctx"
	"gocloud.dev/blob/memblob"
)

func TestGCHandler_Unauthorized(t *testing.T) {
	app := &App{
		Bucket:         memblob.OpenBucket(nil),
		InternalSecret: "secret-key",
	}
	r := httptest.NewRequest(http.MethodPost, "/internal/gc", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	app.gcHandler(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestGCHandler_NoAuthHeader(t *testing.T) {
	app := &App{
		Bucket:         memblob.OpenBucket(nil),
		InternalSecret: "secret-key",
	}
	r := httptest.NewRequest(http.MethodPost, "/internal/gc", nil)
	w := httptest.NewRecorder()
	app.gcHandler(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRunGC_DeletesExpiredFiles(t *testing.T) {
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}
	ctx := testDB.Ctx()
	t.Cleanup(func() { testDB.DeleteFiles(t) })

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

	if n := testDB.CountFiles(t); n != 0 {
		t.Errorf("db files = %d, want 0 after GC", n)
	}
	if n := countObjects(t, bkt); n != 0 {
		t.Errorf("bucket objects = %d, want 0 after GC", n)
	}
}

func TestRunGC_KeepsNonExpiredFiles(t *testing.T) {
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}
	ctx := testDB.Ctx()
	t.Cleanup(func() { testDB.DeleteFiles(t) })

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

	if n := testDB.CountFiles(t); n != 1 {
		t.Errorf("db files = %d, want 1 (non-expired file kept)", n)
	}
	if n := countObjects(t, bkt); n != 1 {
		t.Errorf("bucket objects = %d, want 1 (non-expired object kept)", n)
	}
}

func TestRunGC_AlreadyDeletedFromStorage(t *testing.T) {
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}
	ctx := testDB.Ctx()
	t.Cleanup(func() { testDB.DeleteFiles(t) })

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

	if n := testDB.CountFiles(t); n != 0 {
		t.Errorf("db files = %d, want 0 (orphan DB row cleaned up)", n)
	}
}

func TestRunGC_MixedExpiry(t *testing.T) {
	bkt := newTestBucket(t)
	app := &App{Bucket: bkt}
	ctx := testDB.Ctx()
	t.Cleanup(func() { testDB.DeleteFiles(t) })

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

	if n := testDB.CountFiles(t); n != 1 {
		t.Errorf("db files = %d, want 1 (only non-expired kept)", n)
	}
	if n := countObjects(t, bkt); n != 1 {
		t.Errorf("bucket objects = %d, want 1 (only non-expired kept)", n)
	}
}
