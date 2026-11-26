package legacy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-jet/jet/v2/sqlite"

	"sergeant/internal/database"
	"sergeant/internal/database/jet/table"
	"sergeant/internal/database/schema"
	"sergeant/internal/engine"
)

func initializeFeed(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = sqlite.RawStatement(schema.ShadowTrial).ExecContext(ctx, tx)
	if err != nil {
		return err
	}
	t := table.ShadowTrial
	trialID := sqlite.Func("lower", sqlite.Func("hex", sqlite.Func("randomblob", sqlite.Int(16))))
	initialize := t.INSERT(t.Singleton, t.ID, t.Role).VALUES(1, trialID, "legacy").
		ON_CONFLICT(t.Singleton).DO_NOTHING()
	_, err = initialize.ExecContext(ctx, tx)
	if err != nil {
		return err
	}
	var role string
	roleQuery := t.SELECT(t.Role).WHERE(t.Singleton.EQ(sqlite.Int(1)))
	if err := database.ScanRow(ctx, tx, roleQuery, &role); err != nil {
		return err
	}
	if role != "legacy" {
		return errors.New("primary database is marked as a ledger ghost")
	}
	_, err = sqlite.RawStatement(schema.ShadowQueue).ExecContext(ctx, tx)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// PrepareGhost publishes a consistent copy before the first live command.
// The legacy write lock serializes competing starters. A published copy can
// be recognized after a crash between publishing it and marking legacy ready.
func (l *Engine) PrepareGhost(ctx context.Context, destination string) (string, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	var seeded bool
	t := table.ShadowTrial
	singleton := t.Singleton.EQ(sqlite.Int(1))
	stmt := t.SELECT(t.ID, t.Seeded).WHERE(singleton.AND(t.Role.EQ(sqlite.String("legacy"))))
	if err := database.ScanRow(ctx, tx, stmt, &id, &seeded); err != nil {
		return "", err
	}
	if seeded {
		return id, nil
	}
	_, err = os.Stat(destination)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := l.publishSnapshot(ctx, destination); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	default:
		if err := database.CheckGhost(ctx, destination, id); err != nil {
			return "", fmt.Errorf("existing ghost destination: %w", err)
		}
	}
	if _, err = t.UPDATE(t.Seeded).SET(1).WHERE(singleton).ExecContext(ctx, tx); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (l *Engine) publishSnapshot(ctx context.Context, destination string) error {
	dir := filepath.Dir(destination)
	temp, err := os.CreateTemp(dir, ".sergeant-seed-*")
	if err != nil {
		return err
	}
	path := temp.Name()
	defer func() { _ = os.Remove(path) }()
	if err = temp.Close(); err != nil {
		return err
	}
	// VACUUM INTO includes committed WAL pages. The initialization transaction
	// holds off writers while this second connection takes its read snapshot.
	snapshot := sqlite.RawStatement("VACUUM INTO :path", sqlite.RawArgs{":path": path})
	if _, err := snapshot.ExecContext(ctx, l.db); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", database.FileURI(path, "rw"))
	if err != nil {
		return err
	}
	err = markSnapshot(ctx, db)
	closeErr := db.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Publish without overwriting an existing database.
	if err = os.Link(path, destination); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func markSnapshot(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	t := table.ShadowTrial
	update := t.UPDATE(t.Role, t.Seeded).SET("ledger", 1).WHERE(t.Singleton.EQ(sqlite.Int(1)))
	if _, err := update.ExecContext(ctx, tx); err != nil {
		return err
	}
	if _, err := sqlite.RawStatement("DROP TABLE shadow_queue").ExecContext(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// enqueue uses the live command's transaction so its sequence is the commit order.
func enqueue(ctx context.Context, tx *sql.Tx, delivery engine.Delivery) error {
	payload, err := json.Marshal(delivery.Command)
	if err != nil {
		return err
	}
	var seq int64
	t := table.ShadowTrial
	advance := t.UPDATE(t.Sequence).SET(t.Sequence.ADD(sqlite.Int(1))).
		WHERE(t.Singleton.EQ(sqlite.Int(1)).AND(t.Seeded.EQ(sqlite.Int(1)))).
		RETURNING(t.Sequence)
	err = database.ScanRow(ctx, tx, advance, &seq)
	if err != nil {
		return err
	}
	q := table.ShadowQueue
	appendCommand := q.INSERT(q.Sequence, q.CommandID, q.Author, q.Locale, q.Command).
		VALUES(seq, delivery.ID, delivery.Author, delivery.Locale, payload)
	_, err = appendCommand.ExecContext(ctx, tx)
	return err
}

func (l *Engine) Next(ctx context.Context) (engine.Delivery, bool, error) {
	var d engine.Delivery
	var payload []byte
	q := table.ShadowQueue
	next := q.SELECT(q.Sequence, q.CommandID, q.Author, q.Locale, q.Command).
		ORDER_BY(q.Sequence.ASC()).LIMIT(1)
	err := database.ScanRow(ctx, l.db, next, &d.Sequence, &d.ID, &d.Author, &d.Locale, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	if err = json.Unmarshal(payload, &d.Command); err != nil {
		return d, false, err
	}
	return d, true, nil
}

// Acknowledge removes the payload only after the ledger's checkpoint commits.
func (l *Engine) Acknowledge(ctx context.Context, sequence int64) error {
	q := table.ShadowQueue
	_, err := q.DELETE().WHERE(q.Sequence.EQ(sqlite.Int(sequence))).ExecContext(ctx, l.db)
	return err
}
