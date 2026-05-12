package schema

import (
	"context"
	"database/sql"
	"embed"
)

//go:embed *.sql
var fs embed.FS

func Migrate(ctx context.Context, db *sql.DB) error {
	if err := setupMigrationTable(ctx, db); err != nil {
		return err
	}

	list, err := fs.ReadDir(".")
	if err != nil {
		return err
	}

	for _, x := range list {
		migrateID := x.Name()
		migrated, err := isMigrated(ctx, db, migrateID)
		if err != nil {
			return err
		}
		if migrated {
			continue
		}

		b, err := fs.ReadFile(migrateID)
		if err != nil {
			return err
		}
		if _, err = db.ExecContext(ctx, string(b)); err != nil {
			return err
		}
		if err = stampMigrated(ctx, db, migrateID, string(b)); err != nil {
			return err
		}
	}

	return nil
}

func setupMigrationTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		create table if not exists migrations (
			id      varchar     not null,
			content varchar     not null,
			ts      timestamptz not null default now(),
			primary key (id)
		)
	`)
	return err
}

func isMigrated(ctx context.Context, db *sql.DB, migrateID string) (bool, error) {
	var migrated bool
	err := db.QueryRowContext(ctx, `
		select exists (
			select 1 from migrations where id = $1
		)
	`, migrateID).Scan(&migrated)
	return migrated, err
}

func stampMigrated(ctx context.Context, db *sql.DB, migrateID, content string) error {
	_, err := db.ExecContext(ctx, `
		insert into migrations (id, content)
		values ($1, $2)
		on conflict (id) do nothing
	`, migrateID, content)
	return err
}
