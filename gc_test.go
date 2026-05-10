package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

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

