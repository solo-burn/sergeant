package main

import (
	"context"
	"database/sql"
	_ "embed"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/go-jet/jet/v2/sqlite"

	"sergeant/internal/database"
	"sergeant/internal/database/jet/table"
	"sergeant/internal/i18n"
)

//go:embed dashboard.html
var dashboardHTML string

var dashboardPage = template.Must(template.New("ledger").Parse(dashboardHTML))

func openDashboardDB(path string) (*sql.DB, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return sql.Open("sqlite", database.FileURI(path, "ro"))
}

const entriesPerPage = 32

type ledgerEntry struct {
	ID, RecordedAt, Amount                           int64
	Kind, Creditor, Debtor, Currency, Actor, Command string
}

type ledgerPage struct {
	Metrics      string
	Entries      []ledgerEntry
	Older, Newer int64
	Live         bool
}

func (ledgerPage) Limit() int { return entriesPerPage }

func cursorID(query url.Values, key string) (int64, bool) {
	if !query.Has(key) {
		return 0, true
	}
	id, err := strconv.ParseInt(query.Get(key), 10, 64)
	return id, err == nil && id >= 0
}

func dashboardHandler(db *sql.DB) http.Handler {
	var totals ledgerTotals
	tr := i18n.New(os.Getenv("SERGEANT_LOCALE"))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		before, validBefore := cursorID(query, "before")
		after, validAfter := cursorID(query, "after")
		if !validBefore || !validAfter || (query.Has("before") && query.Has("after")) {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}

		page := ledgerPage{Live: before == 0 && after == 0}
		lastID, metrics, err := totals.snapshot(r.Context(), db, tr)
		var entries []ledgerEntry
		var more bool
		if err == nil {
			page.Metrics = metrics
			entries, more, err = readEntries(r.Context(), db, lastID, before, after, after != 0)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case err != nil:
			w.WriteHeader(http.StatusServiceUnavailable)
		case len(entries) > 0:
			page.Entries = entries
			if before != 0 || (after != 0 && more) {
				page.Newer = entries[0].ID
			}
			if after != 0 || more {
				page.Older = entries[len(entries)-1].ID
			}
		case !page.Live:
			http.NotFound(w, r)
			return
		}
		_ = dashboardPage.Execute(w, page)
	})
	mux.HandleFunc("GET /entries", func(w http.ResponseWriter, r *http.Request) {
		since, valid := cursorID(r.URL.Query(), "since")
		if !valid {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
		lastID, metrics, err := totals.snapshot(r.Context(), db, tr)
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if lastID <= since {
			if since == 0 {
				w.Header().Set("X-Ledger-Metrics", metrics)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		entries, more, err := readEntries(r.Context(), db, lastID, 0, since, false)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Ledger-Metrics", metrics)
		if more {
			w.Header().Set("X-Ledger-Has-Older", "1")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = dashboardPage.ExecuteTemplate(w, "rows", entries)
	})
	return mux
}

// A newer-page link needs the closest 32 IDs above its cursor. A live poll needs the most recent 32.
func readEntries(
	ctx context.Context, db *sql.DB, lastID, before, after int64, ascending bool,
) ([]ledgerEntry, bool, error) {
	e := table.LedgerEntries
	stmt := e.SELECT(e.ID, e.RecordedAt, e.Kind, e.CreditorID, e.DebtorID, e.Currency, e.AmountMinor,
		sqlite.COALESCE(e.ActorID, sqlite.String("-")), sqlite.COALESCE(e.CommandID, sqlite.String("-"))).
		LIMIT(entriesPerPage + 1)
	filter := e.ID.LT_EQ(sqlite.Int(lastID))
	if before > 0 {
		filter = filter.AND(e.ID.LT(sqlite.Int(before)))
	} else if after > 0 {
		filter = filter.AND(e.ID.GT(sqlite.Int(after)))
	}
	order := e.ID.DESC()
	if ascending {
		order = e.ID.ASC()
	}
	rows, err := stmt.WHERE(filter).ORDER_BY(order).Rows(ctx, db)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var entries []ledgerEntry
	more := false
	for rows.Next() {
		if len(entries) == entriesPerPage {
			more = true
			break
		}
		var row ledgerEntry
		err := rows.Rows.Scan(&row.ID, &row.RecordedAt, &row.Kind, &row.Creditor, &row.Debtor,
			&row.Currency, &row.Amount, &row.Actor, &row.Command)
		if err != nil {
			return nil, false, err
		}
		entries = append(entries, row)
	}
	if ascending {
		slices.Reverse(entries)
	}
	return entries, more, rows.Err()
}
