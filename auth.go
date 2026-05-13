package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/moonrhythm/cachestore"
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

	cacheKey := "dropbox|auth|" + auth + "|" + project + "|" + projectID
	if v, ok := cachestore.Get[AuthResult](cacheKey); ok {
		return v
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

	req, _ := http.NewRequest(http.MethodPost, apiEndpoint+"/me.authorized", bytes.NewReader(body))
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return AuthResult{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return AuthResult{}
	}

	var res struct {
		OK     bool `json:"ok"`
		Result struct {
			Authorized bool `json:"authorized"`
			Project    struct {
				ID      string `json:"id"`
				Project string `json:"project"`
				BillingAccount struct {
					Active bool `json:"active"`
				} `json:"billingAccount"`
			} `json:"project"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return AuthResult{}
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
	return result
}
