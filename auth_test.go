package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// authServer returns a test server that responds as the deploys.app auth API.
func authServer(authorized, billingActive bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
}

func withAPIEndpoint(t *testing.T, url string) {
	t.Helper()
	orig := apiEndpoint
	apiEndpoint = url
	t.Cleanup(func() { apiEndpoint = orig })
}

func TestCheckAuth_AlphaMode(t *testing.T) {
	result := checkAuth(context.Background(), "", "", "")
	if !result.Authorized {
		t.Fatal("expected authorized in alpha mode (no auth header)")
	}
	if result.Project.ID != "alpha" {
		t.Errorf("project ID = %q, want alpha", result.Project.ID)
	}
}

func TestCheckAuth_NoProject(t *testing.T) {
	result := checkAuth(context.Background(), "Bearer token", "", "")
	if result.Authorized {
		t.Error("expected unauthorized when project and projectId are both empty")
	}
}

func TestCheckAuth_Authorized(t *testing.T) {
	srv := authServer(true, true)
	defer srv.Close()
	withAPIEndpoint(t, srv.URL)

	result := checkAuth(context.Background(), "Bearer "+t.Name(), "myproject", "")
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
	srv := authServer(false, true)
	defer srv.Close()
	withAPIEndpoint(t, srv.URL)

	result := checkAuth(context.Background(), "Bearer "+t.Name(), "myproject", "")
	if result.Authorized {
		t.Error("expected unauthorized when API returns authorized=false")
	}
}

func TestCheckAuth_BillingInactive(t *testing.T) {
	srv := authServer(true, false)
	defer srv.Close()
	withAPIEndpoint(t, srv.URL)

	result := checkAuth(context.Background(), "Bearer "+t.Name(), "myproject", "")
	if result.Authorized {
		t.Error("expected unauthorized when billing is inactive")
	}
}

func TestCheckAuth_ProjectFromID(t *testing.T) {
	srv := authServer(true, true)
	defer srv.Close()
	withAPIEndpoint(t, srv.URL)

	result := checkAuth(context.Background(), "Bearer "+t.Name(), "", "proj-123")
	if !result.Authorized {
		t.Error("expected authorized using projectId instead of project name")
	}
}

func TestCheckAuth_APIDown(t *testing.T) {
	withAPIEndpoint(t, "http://127.0.0.1:0")

	result := checkAuth(context.Background(), "Bearer "+t.Name(), "myproject", "")
	if result.Authorized {
		t.Error("expected unauthorized when auth API is unreachable")
	}
}

func TestCheckAuth_CachesResult(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"authorized": true,
				"project": map[string]any{
					"id": "proj-123", "project": "myproject",
					"billingAccount": map[string]any{"active": true},
				},
			},
		})
	}))
	defer srv.Close()
	withAPIEndpoint(t, srv.URL)

	token := "Bearer " + t.Name()
	checkAuth(context.Background(), token, "myproject", "")
	checkAuth(context.Background(), token, "myproject", "")

	if calls != 1 {
		t.Errorf("auth API called %d times, want 1 (should be cached)", calls)
	}
}
