// Package tu is the test utility.
package tu

import (
	"context"
	"database/sql"

	"github.com/acoshift/pgsql/pgctx"
	"github.com/cockroachdb/cockroach-go/v2/testserver"
)

// Context holds the test server and DB connection.
type Context struct {
	ts testserver.TestServer
	DB *sql.DB
}

func (c *Context) setup() {
	var err error
	defer func() {
		if err != nil {
			panic(err)
		}
	}()

	c.ts, err = testserver.NewTestServer()
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			c.Teardown()
		}
	}()

	c.DB, err = sql.Open("postgres", c.ts.PGURL().String()+"&enable_implicit_transaction_for_batch_statements=off")
	if err != nil {
		return
	}

	err = migrate(context.Background(), c.DB)
}

func migrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS files (
			fn         TEXT        NOT NULL,
			project_id TEXT        NOT NULL,
			size       BIGINT      NOT NULL,
			filename   TEXT        NOT NULL,
			ttl        INTEGER     NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS files_project_id_created_at_idx ON files (project_id, created_at);
	`)
	return err
}

func (c *Context) Teardown() {
	if c.DB != nil {
		c.DB.Close()
	}
	if c.ts != nil {
		c.ts.Stop()
	}
}

// Ctx returns a context with the DB injected.
func (c *Context) Ctx() context.Context {
	return pgctx.NewContext(context.Background(), c.DB)
}

// Setup starts a CockroachDB test server and runs the schema migration.
func Setup() *Context {
	c := &Context{}
	c.setup()
	return c
}

// DeleteFiles removes all rows from the files table. Call from t.Cleanup to isolate tests.
func (c *Context) DeleteFiles(t interface{ Helper(); Fatal(...any) }) {
	t.Helper()
	if _, err := c.DB.ExecContext(context.Background(), `DELETE FROM files`); err != nil {
		t.Fatal(err)
	}
}

// CountFiles returns the number of rows in the files table.
func (c *Context) CountFiles(t interface{ Helper(); Fatal(...any) }) int {
	t.Helper()
	var n int
	if err := c.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM files`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
