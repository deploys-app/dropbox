package main

import (
	"testing"

	"moonrhythm/dropbox/tu"
)

// newTestDB starts a fresh CockroachDB instance for the test and tears it down
// on completion. Each test gets its own isolated database so they can run in
// parallel without sharing rows or worrying about cleanup.
func newTestDB(t *testing.T) *tu.Context {
	t.Helper()
	c := tu.Setup()
	t.Cleanup(c.Teardown)
	return c
}
