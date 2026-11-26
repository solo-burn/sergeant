package database

import (
	"context"
	"database/sql"

	"github.com/go-jet/jet/v2/qrm"
	"github.com/go-jet/jet/v2/sqlite"
)

// ScanRow reads a single Jet result by position. A missing receipt or queue
// entry must remain distinguishable from a database error.
func ScanRow(ctx context.Context, db qrm.Queryable, stmt sqlite.Statement, dest ...any) error {
	rows, err := stmt.Rows(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if err := rows.Rows.Scan(dest...); err != nil {
		return err
	}
	return rows.Close()
}
