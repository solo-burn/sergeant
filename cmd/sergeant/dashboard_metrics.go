package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"sync"

	"github.com/go-jet/jet/v2/sqlite"

	"sergeant/internal/database/jet/table"
	"sergeant/internal/i18n"
)

// Journal entries are immutable. After the first scan, only new entries contribute to the totals.
type ledgerTotals struct {
	mu             sync.Mutex
	lastID, events int64
	pln, eur       big.Int
}

func (t *ledgerTotals) snapshot(ctx context.Context, db *sql.DB, tr *i18n.Translator) (int64, string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	e := table.LedgerEntries
	stmt := e.SELECT(e.ID, e.Kind, e.Currency, e.AmountMinor).
		WHERE(e.ID.GT(sqlite.Int(t.lastID))).ORDER_BY(e.ID.ASC())
	rows, err := stmt.Rows(ctx, db)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, minor int64
		var kind, currency string
		if err := rows.Rows.Scan(&id, &kind, &currency, &minor); err != nil {
			return 0, "", err
		}
		t.events++
		t.lastID = id
		if kind == "opening" {
			continue
		}
		switch currency {
		case "PLN":
			t.pln.Add(&t.pln, big.NewInt(minor))
		case "EUR":
			t.eur.Add(&t.eur, big.NewInt(minor))
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	line := tr.T(i18n.DashboardMetrics, t.events, formatTurnover(&t.pln), formatTurnover(&t.eur))
	return t.lastID, line, nil
}

func formatTurnover(minor *big.Int) string {
	var whole, cents big.Int
	whole.QuoRem(minor, big.NewInt(100), &cents)
	return fmt.Sprintf("%s.%02d", whole.String(), cents.Int64())
}
