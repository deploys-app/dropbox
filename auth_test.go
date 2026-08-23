package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A single mock server is started lazily and serves every auth test. Each test
// registers a handler keyed by its Authorization header so parallel tests do
// not race over the package-level apiEndpoint variable.
var (
	authMockOnce sync.Once
	authMockMap  sync.Map // token -> http.HandlerFunc
)

func setupAuthMock() {
	authMockOnce.Do(func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h, ok := authMockMap.Load(r.Header.Get("Authorization")); ok {
				h.(http.HandlerFunc)(w, r)
				return
			}
			http.Error(w, "no mock registered for token", http.StatusNotFound)
		}))
		apiEndpoint = srv.URL
	})
}

func registerAuthMock(t *testing.T, token string, h http.HandlerFunc) {
	t.Helper()
	setupAuthMock()
	authMockMap.Store(token, h)
	t.Cleanup(func() { authMockMap.Delete(token) })
}

// jsonAuthMock returns an http.HandlerFunc that responds with the standard
// authorize JSON envelope.
func jsonAuthMock(authorized, billingActive bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"authorized": authorized,
				"project": map[string]any{
					"id":      "proj-123",
					"project": "myproject",
					"billingAccount": map[string]any{
						"active": billingActive,
					},
				},
			},
		})
	}
}

func TestCheckAuth_AlphaMode(t *testing.T) {
	t.Parallel()
	result := checkAuth(context.Background(), "", "", "")
	if !result.Authorized {
		t.Fatal("expected authorized in alpha mode (no auth header)")
	}
	if result.Project.ID != "alpha" {
		t.Errorf("project ID = %q, want alpha", result.Project.ID)
	}
}

func TestCheckAuth_NoProject(t *testing.T) {
	t.Parallel()
	result := checkAuth(context.Background(), "Bearer "+t.Name(), "", "")
	if result.Authorized {
		t.Error("expected unauthorized when project and projectId are both empty")
	}
}

func TestCheckAuth_Authorized(t *testing.T) {
	t.Parallel()
	token := "Bearer " + t.Name()
	registerAuthMock(t, token, jsonAuthMock(true, true))

	result := checkAuth(context.Background(), token, "myproject", "")
	if !result.Authorized {
		t.Fatal("expected authorized")
	}
	if result.Project.ID != "proj-123" {
		t.Errorf("project ID = %q, want proj-123", result.Project.ID)
	}
	if result.Project.Project != "myproject" {
		t.Errorf("project = %q, want myproject", result.Project.Project)
	}
}

func TestCheckAuth_NotAuthorized(t *testing.T) {
	t.Parallel()
	token := "Bearer " + t.Name()
	registerAuthMock(t, token, jsonAuthMock(false, true))

	result := checkAuth(context.Background(), token, "myproject", "")
	if result.Authorized {
		t.Error("expected unauthorized when API returns authorized=false")
	}
}

func TestCheckAuth_BillingInactive(t *testing.T) {
	t.Parallel()
	token := "Bearer " + t.Name()
	registerAuthMock(t, token, jsonAuthMock(true, false))

	result := checkAuth(context.Background(), token, "myproject", "")
	if result.Authorized {
		t.Error("expected unauthorized when billing is inactive")
	}
}

func TestCheckAuth_ProjectFromID(t *testing.T) {
	t.Parallel()
	token := "Bearer " + t.Name()
	registerAuthMock(t, token, jsonAuthMock(true, true))

	result := checkAuth(context.Background(), token, "", "proj-123")
	if !result.Authorized {
		t.Error("expected authorized using projectId instead of project name")
	}
}

func TestCheckAuth_APIError(t *testing.T) {
	t.Parallel()
	token := "Bearer " + t.Name()
	// Hijack and close the connection so net/http surfaces a transport error.
	registerAuthMock(t, token, func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter is not a Hijacker")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	})

	result := checkAuth(context.Background(), token, "myproject", "")
	if result.Authorized {
		t.Error("expected unauthorized when auth API connection fails")
	}
}

func TestCheckAuth_API500(t *testing.T) {
	t.Parallel()
	token := "Bearer " + t.Name()
	registerAuthMock(t, token, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	result := checkAuth(context.Background(), token, "myproject", "")
	if result.Authorized {
		t.Error("expected unauthorized when auth API returns 500")
	}
}

func TestCheckAuth_APIInvalidJSON(t *testing.T) {
	t.Parallel()
	token := "Bearer " + t.Name()
	registerAuthMock(t, token, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})

	result := checkAuth(context.Background(), token, "myproject", "")
	if result.Authorized {
		t.Error("expected unauthorized when auth API returns invalid JSON")
	}
}

func TestCheckAuth_APIOKFalse(t *testing.T) {
	t.Parallel()
	token := "Bearer " + t.Name()
	registerAuthMock(t, token, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": false,
			"result": map[string]any{
				"authorized": true,
				"project": map[string]any{
					"id":             "proj-123",
					"project":        "myproject",
					"billingAccount": map[string]any{"active": true},
				},
			},
		})
	})

	result := checkAuth(context.Background(), token, "myproject", "")
	if result.Authorized {
		t.Error("expected unauthorized when API envelope has ok=false")
	}
}

func TestCheckAuth_CachesResult(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	token := "Bearer " + t.Name()
	registerAuthMock(t, token, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		jsonAuthMock(true, true)(w, r)
	})

	checkAuth(context.Background(), token, "myproject", "")
	checkAuth(context.Background(), token, "myproject", "")

	if got := calls.Load(); got != 1 {
		t.Errorf("auth API called %d times, want 1 (should be cached)", got)
	}
}

func TestCheckAuth_SingleflightCollapsesConcurrentCalls(t *testing.T) {
	// Same shape as TestLookupFile_SingleflightCollapsesConcurrentCalls:
	// 50 goroutines race on a cold-cache (auth, project, projectId)
	// triple. sf.Do must collapse them into a single /me.authorized
	// round-trip — otherwise a thundering herd of uploads from one
	// caller (e.g. parallel CI jobs holding the same bearer) hammers
	// the deploys.app API on every cache-miss edge.
	t.Parallel()
	var calls atomic.Int64
	token := "Bearer " + t.Name()
	registerAuthMock(t, token, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// A short sleep widens the singleflight window so the test
		// reliably catches a regression where dedupe is broken.
		time.Sleep(50 * time.Millisecond)
		jsonAuthMock(true, true)(w, r)
	})

	const N = 50
	results := make([]AuthResult, N)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range N {
		wg.Go(func() {
			<-start
			results[i] = checkAuth(context.Background(), token, "sfproject", "")
		})
	}
	close(start)
	wg.Wait()

	for i, r := range results {
		if !r.Authorized {
			t.Errorf("results[%d] not authorized", i)
		}
	}
	if got := calls.Load(); got >= N {
		t.Errorf("auth API called %d times, want <%d (sf should have collapsed the herd)", got, N)
	}
}

func TestCheckAuth_CacheKeyDistinguishesTokens(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	t1 := "Bearer " + t.Name() + "/A"
	t2 := "Bearer " + t.Name() + "/B"
	for _, tok := range []string{t1, t2} {
		registerAuthMock(t, tok, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			jsonAuthMock(true, true)(w, r)
		})
	}

	checkAuth(context.Background(), t1, "myproject", "")
	checkAuth(context.Background(), t2, "myproject", "")
	checkAuth(context.Background(), t1, "myproject", "") // cached
	checkAuth(context.Background(), t2, "myproject", "") // cached

	if got := calls.Load(); got != 2 {
		t.Errorf("auth API called %d times, want 2 (one per distinct token)", got)
	}
}
