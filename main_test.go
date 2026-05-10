package main

import (
	"os"
	"testing"

	"moonrhythm/dropbox/tu"
)

var testDB *tu.Context

func TestMain(m *testing.M) {
	testDB = tu.Setup()
	code := m.Run()
	testDB.Teardown()
	os.Exit(code)
}
