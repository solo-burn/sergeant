package store

import (
	"context"
	"fmt"

	"github.com/go-jet/jet/v2/sqlite"

	"sergeant/internal/database"
	"sergeant/internal/database/jet/table"
	"sergeant/internal/engine"
)

// Replay commits the command and advances the ghost checkpoint together.
// Two workers or a crash before queue acknowledgement can repeat a delivery.
func (s *SQLite) Replay(
	ctx context.Context,
	d engine.Delivery,
	apply func(*Transaction) (string, error),
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var previous int64
	trial := table.ShadowTrial
	singleton := trial.Singleton.EQ(sqlite.Int(1))
	ledger := singleton.AND(trial.Role.EQ(sqlite.String("ledger")))
	checkpoint := trial.SELECT(trial.Sequence).WHERE(ledger)
	if err := database.ScanRow(ctx, tx, checkpoint, &previous); err != nil {
		return err
	}
	if d.Sequence <= previous {
		return nil
	}
	if d.Sequence != previous+1 {
		return fmt.Errorf("ghost at %d received command %d", previous, d.Sequence)
	}
	text, resultErr := apply(&Transaction{queries{conn: tx, commandID: d.ID}})
	outcome, terminal := engine.Outcome(resultErr)
	if !terminal {
		return resultErr
	}
	if d.ID != "" && d.Command.Mutates() {
		_, err := insertReceipt(d.ID, text, outcome).ExecContext(ctx, tx)
		if err != nil {
			return err
		}
	}
	update := trial.UPDATE(trial.Sequence).SET(d.Sequence).WHERE(singleton)
	_, err = update.ExecContext(ctx, tx)
	if err != nil {
		return err
	}
	return tx.Commit()
}
