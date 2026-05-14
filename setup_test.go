package main

import "moonrhythm/dropbox/tu"

// testDB is the shared CockroachDB used by tests. It replaces the TestMain entry
// point so individual tests can declare t.Parallel(); the OS reclaims the test
// server when the test binary exits.
var testDB = tu.Default()
