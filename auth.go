package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/moonrhythm/cachestore"
	"github.com/moonrhythm/sf"
)

var apiEndpoint = "https://api.deploys.app"

const (
	cacheTTL   = 30 * time.Second
	permission = "dropbox.upload"
)

type Project struct {
	ID      string
	Project string
}

type AuthResult struct {
	Authorized bool
	Project    Project
}

func checkAuth(ctx context.Context, auth, project, projectID string) AuthResult {
	if auth == "" {
		// TODO: remove after alpha
		return AuthResult{
			Authorized: true,
			Project:    Project{ID: "alpha", Project: "alpha"},
		}
	}
	if project == "" && projectID == "" {
		return AuthResult{}
	}

	cacheKey := "auth|" + auth + "|" + project + "|" + projectID
	if v, ok := cachestore.Get[AuthResult](cacheKey); ok {
		return v
	}

	// Same singleflight pattern as lookupFile: collapse a thundering herd
	// of concurrent uploads from the same caller into a single
	// /me.authorized round-trip. The result is cached for 30s
	// (cacheTTL); sf.Do dedupe matters at the cold-cache edge and right
	// after that 30s entry expires under load.
	result, _, _ := sf.Do(ctx, cacheKey, func(ctx context.Context) (AuthResult, error) {
		// Re-check the cache: a sibling caller may have populated it
		// while we were queued behind sf's mutex.
		if v, ok := cachestore.Get[AuthResult](cacheKey); ok {
			return v, nil
		}

		body, _ := json.Marshal(struct {
			Project     string   `json:"project,omitempty"`
			ProjectID   string   `json:"projectId,omitempty"`
			Permissions []string `json:"permissions"`
		}{
			Project:     project,
			ProjectID:   projectID,
			Permissions: []string{permission},
		})

		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiEndpoint+"/me.authorized", bytes.NewReader(body))
		req.Header.Set("Authorization", auth)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			// Don't cache transport failures — let the next caller retry.
			return AuthResult{}, nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return AuthResult{}, nil
		}

		var res struct {
			OK     bool `json:"ok"`
			Result struct {
				Authorized bool `json:"authorized"`
				Project    struct {
					ID             string `json:"id"`
					Project        string `json:"project"`
					BillingAccount struct {
						Active bool `json:"active"`
					} `json:"billingAccount"`
				} `json:"project"`
			} `json:"result"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			return AuthResult{}, nil
		}

		var result AuthResult
		if res.OK && res.Result.Authorized && res.Result.Project.BillingAccount.Active {
			result = AuthResult{
				Authorized: true,
				Project: Project{
					ID:      res.Result.Project.ID,
					Project: res.Result.Project.Project,
				},
			}
		}

		cachestore.Set(cacheKey, result, &cachestore.SetOptions{TTL: cacheTTL})
		return result, nil
	})
	return result
}
