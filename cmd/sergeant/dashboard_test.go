package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sergeant/internal/database"
	"sergeant/internal/database/schema"
	"sergeant/internal/i18n"
	"sergeant/internal/parser"
	"sergeant/internal/shadow"
)

func dashboardDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.OpenBalances(":memory:", schema.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestDashboardShowsRawOperationsNewestFirst(t *testing.T) {
	// Arrange
	db := dashboardDB(t)
	_, err := db.Exec(`INSERT INTO ledger_entries
		(id, recorded_at, kind, creditor_id, debtor_id, currency, amount_minor, actor_id, command_id) VALUES
		(101, 1795644000, 'opening',    'U01', 'U02', 'PLN', 4000, NULL,  NULL),
		(102, 1795644060, 'adjustment', 'U01', 'U02', 'PLN', 2000, 'U01', 'event/Ev91/0'),
		(103, 1795644120, 'reset',      'U02', 'U01', 'PLN', 6000, 'U01', 'event/Ev92/0')`)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	entries, more, err := readEntries(t.Context(), db, 103, 0, 0, false)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	want := []ledgerEntry{
		{103, 1795644120, 6000, "reset", "U02", "U01", "PLN", "U01", "event/Ev92/0"},
		{102, 1795644060, 2000, "adjustment", "U01", "U02", "PLN", "U01", "event/Ev91/0"},
		{101, 1795644000, 4000, "opening", "U01", "U02", "PLN", "-", "-"},
	}
	if more || !slices.Equal(entries, want) {
		t.Fatalf("operations = %+v, want %+v", entries, want)
	}
}

func seedEntries(t *testing.T, db *sql.DB, count int) {
	t.Helper()
	_, err := db.Exec(`WITH RECURSIVE ids(id) AS (VALUES(1) UNION ALL SELECT id+1 FROM ids WHERE id<?)
		INSERT INTO ledger_entries(kind, creditor_id, debtor_id, currency, amount_minor)
		SELECT 'opening', 'A', 'B', 'PLN', 1 FROM ids`, count)
	if err != nil {
		t.Fatal(err)
	}
}

func checkIDs(t *testing.T, html string, first, last int) {
	t.Helper()
	rows := regexp.MustCompile(`<tr data-id="([0-9]+)">`).FindAllStringSubmatch(html, -1)
	if len(rows) != first-last+1 {
		t.Fatalf("page has %d rows, want %d", len(rows), first-last+1)
	}
	for i, match := range rows {
		if match[1] != strconv.Itoa(first-i) {
			t.Fatalf("row %d has ID %s, want %d", i, match[1], first-i)
		}
	}
}

func TestLivePageShowsOlderLinkOnlyWhenThereIsMoreHistory(t *testing.T) {
	for _, count := range []int{32, 33} {
		t.Run(fmt.Sprintf("%d entries", count), func(t *testing.T) {
			// Arrange
			db := dashboardDB(t)
			seedEntries(t, db, count)
			handler := newDashboard(db)
			res := httptest.NewRecorder()

			// Act
			handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))

			// Assert
			html := res.Body.String()
			checkIDs(t, html, count, count-31)
			if strings.Contains(html, `href="/?before=2"`) != (count == 33) {
				t.Fatalf("unexpected older-page link for %d entries", count)
			}
		})
	}
}

func startLiveDashboard(t *testing.T) (*shadow.Trial, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.db")
	trial, err := shadow.Open(t.Context(), filepath.Join(dir, "legacy.db"), path, "PLN", i18n.New("en"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = trial.Legacy.Close() })
	db, err := openDashboardDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	dashboard := newDashboard(db)
	trial.LedgerChanged = dashboard.notifyLedgerChanged
	server := httptest.NewServer(dashboard)
	server.Client().Timeout = 5 * time.Second
	t.Cleanup(server.Close)

	var replay sync.WaitGroup
	replay.Go(func() { trial.Run(t.Context(), slog.New(slog.DiscardHandler)) })
	t.Cleanup(replay.Wait)
	return trial, server
}

func TestDashboardStreamsCommittedBalanceChanges(t *testing.T) {
	// Arrange
	trial, server := startLiveDashboard(t)
	stream, err := server.Client().Get(server.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()
	lines := bufio.NewScanner(stream.Body)
	charge := parser.Command{Kind: parser.KindAdd, Target: "B", Sign: 1, Minor: 100}

	// Act
	_, err = trial.Legacy.ApplyOnce(t.Context(), "charge", "A", charge)
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	if got := stream.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("stream content type = %q", got)
	}
	for lines.Scan() {
		if lines.Text() == "event: ledger-changed" {
			break
		}
	}
	if !lines.Scan() || !strings.HasPrefix(lines.Text(), "data:") {
		t.Fatalf("missing ledger-changed event data: %q (stream error: %v)", lines.Text(), lines.Err())
	}
	res := httptest.NewRecorder()
	server.Config.Handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/entries?since=0", nil))
	checkIDs(t, res.Body.String(), 1, 1)
}

func TestLiveUpdateReturnsOnlyNewEntries(t *testing.T) {
	// Arrange
	db := dashboardDB(t)
	seedEntries(t, db, 40)
	handler := newDashboard(db)
	res := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/entries?since=38", nil))

	// Assert
	checkIDs(t, res.Body.String(), 40, 39)
}

func TestLiveUpdateWithNoChangesReturnsNoContent(t *testing.T) {
	// Arrange
	db := dashboardDB(t)
	seedEntries(t, db, 40)
	handler := newDashboard(db)
	res := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/entries?since=40", nil))

	// Assert
	if res.Code != http.StatusNoContent || res.Body.Len() != 0 {
		t.Fatalf("idle update: %d, %d bytes", res.Code, res.Body.Len())
	}
}

func TestLiveUpdateAfterLongPauseReturnsOnlyNewestPage(t *testing.T) {
	// Arrange
	db := dashboardDB(t)
	seedEntries(t, db, 100)
	handler := newDashboard(db)
	res := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/entries?since=1", nil))

	// Assert
	checkIDs(t, res.Body.String(), 100, 69)
	if res.Header().Get("X-Ledger-Has-Older") != "1" {
		t.Fatal("catch-up response omitted the older-page indicator")
	}
}

func TestHistoryPaginationWhileNewEntriesArrive(t *testing.T) {
	for _, tc := range []struct {
		name, path  string
		first, last int
	}{
		{"older page stays anchored", "/?before=34", 33, 2},
		{"newer page stays adjacent", "/?after=33", 65, 34},
		{"oldest page has remaining entry", "/?before=2", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			db := dashboardDB(t)
			seedEntries(t, db, 65)
			seedEntries(t, db, 2)
			handler := newDashboard(db)
			res := httptest.NewRecorder()

			// Act
			handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, tc.path, nil))

			// Assert
			html := res.Body.String()
			checkIDs(t, html, tc.first, tc.last)
			if strings.Contains(html, "<script>") {
				t.Fatal("historical page subscribes to the live feed")
			}
		})
	}
}

func TestDashboardShowsPromptWhileWaitingForEntries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		open   func(*testing.T) *sql.DB
		status int
	}{
		{"empty ledger", dashboardDB, http.StatusOK},
		{"missing ledger", missingDashboardDB, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			handler := newDashboard(tc.open(t))
			res := httptest.NewRecorder()

			// Act
			handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))

			// Assert
			const prompt = `>$<span class="cursor">_</span>`
			if res.Code != tc.status || !strings.Contains(res.Body.String(), prompt) {
				t.Fatalf("response = %d %s", res.Code, res.Body)
			}
		})
	}
}

func missingDashboardDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "missing.db")
	db, err := sql.Open("sqlite", database.FileURI(path, "ro"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMetricsCoverWholeLedgerOnHistoricalPage(t *testing.T) {
	// Arrange
	t.Setenv("SERGEANT_LOCALE", "en")
	db := dashboardDB(t)
	seedEntries(t, db, 40)
	_, err := db.Exec(`INSERT INTO ledger_entries
		(id, kind, creditor_id, debtor_id, currency, amount_minor, actor_id) VALUES
		(101, 'adjustment', 'C', 'D', 'PLN', 2450, 'C'),
		(102, 'reset',      'D', 'C', 'PLN', 2450, 'D'),
		(103, 'adjustment', 'C', 'D', 'EUR', 99,   'C'),
		(104, 'adjustment', 'C', 'D', 'USD', 500,  'C')`)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()

	// Act
	newDashboard(db).ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/?before=10", nil))

	// Assert
	want := `<pre id="metrics">events 44, moved PLN 49.00 EUR 0.99</pre>`
	if !strings.Contains(res.Body.String(), want) {
		t.Fatalf("missing ledger-wide metrics %q", want)
	}
}

func TestLiveMetricsAccumulateNewEntriesBeyondInt64(t *testing.T) {
	// Arrange
	t.Setenv("SERGEANT_LOCALE", "en")
	db := dashboardDB(t)
	insert := `INSERT INTO ledger_entries(kind, creditor_id, debtor_id, currency, amount_minor, actor_id)
		VALUES ('adjustment', 'A', ?, 'EUR', ?, 'A')`
	if _, err := db.Exec(insert, "B", int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	handler := newDashboard(db)
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("initial page status = %d", first.Code)
	}
	if _, err := db.Exec(insert, "C", 1); err != nil {
		t.Fatal(err)
	}
	update := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(update, httptest.NewRequest(http.MethodGet, "/entries?since=1", nil))

	// Assert
	want := "events 2, moved PLN 0.00 EUR 92233720368547758.08"
	if got := update.Header().Get("X-Ledger-Metrics"); got != want {
		t.Fatalf("live metrics = %q, want %q", got, want)
	}
}
