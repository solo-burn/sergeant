package shadow

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"sergeant/internal/database"
	"sergeant/internal/database/schema"
	"sergeant/internal/engine"
	"sergeant/internal/i18n"
	"sergeant/internal/ledger"
	"sergeant/internal/parser"
)

type trialFixture struct {
	*Trial
	primary, ghost string
}

func openTrial(t *testing.T, primary, ghost string) trialFixture {
	t.Helper()
	trial, err := Open(t.Context(), primary, ghost, "PLN", i18n.New("en"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = trial.Legacy.Close() })
	return trialFixture{trial, primary, ghost}
}

func newTrial(t *testing.T) trialFixture {
	t.Helper()
	dir := t.TempDir()
	return openTrial(t, filepath.Join(dir, "legacy.db"), filepath.Join(dir, "ledger.db"))
}

func (f trialFixture) ledger(t *testing.T) *ledger.Ledger {
	t.Helper()
	s, err := f.openLedger(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return ledger.New(s, "PLN", i18n.New("en"))
}

func (f trialFixture) apply(t *testing.T, id string, cmd parser.Command) engine.Reply {
	t.Helper()
	r, err := f.Legacy.ApplyOnce(t.Context(), id, "A", cmd)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func assertReplaySequence(t *testing.T, path string, want int64) {
	t.Helper()
	var got int64
	if err := connect(t, path).QueryRow("SELECT sequence FROM shadow_trial").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("replay sequence = %d, want %d", got, want)
	}
}

func connect(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", database.FileURI(path, "rw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func charge(amount int64) parser.Command {
	return parser.Command{Kind: parser.KindAdd, Target: "B", Sign: 1, Minor: amount}
}

func TestRestartLeavesCurrentLegacyDatabaseUntouched(t *testing.T) {
	// Arrange
	f := newTrial(t)
	db := connect(t, f.primary)
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	dataVersion := func() int {
		t.Helper()
		var version int
		if err := conn.QueryRowContext(t.Context(), "PRAGMA data_version").Scan(&version); err != nil {
			t.Fatal(err)
		}
		return version
	}
	before := dataVersion()

	// Act
	openTrial(t, f.primary, f.ghost)

	// Assert
	if after := dataVersion(); after != before {
		t.Fatalf("restart wrote to the legacy database (data_version %d -> %d)", before, after)
	}
}

func TestTrialStartsFromLegacySnapshot(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	primary := filepath.Join(dir, "production?#.db")
	db, err := database.OpenBalances(primary)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`INSERT INTO debts VALUES('A','B','EUR',3956,100,101);
		INSERT INTO payment_methods VALUES ('A','custom','sample',20,21,1);
		INSERT INTO applied_commands VALUES ('existing','original reply','applied');
		PRAGMA user_version=0;`)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	f := openTrial(t, primary, filepath.Join(dir, "ghost.db"))
	f.ledger(t)

	// Assert
	assertReplaySequence(t, f.ghost, 0)
	var journalTables, openings int
	tables := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name='ledger_entries'")
	if err := tables.Scan(&journalTables); err != nil || journalTables != 0 {
		t.Fatalf("legacy has %d journal tables: %v", journalTables, err)
	}
	other := connect(t, f.ghost)
	seeded := other.QueryRow(`SELECT count(*) FROM ledger_entries
		WHERE kind='opening' AND amount_minor=3956 AND recorded_at=101`)
	if err := seeded.Scan(&openings); err != nil || openings != 1 {
		t.Fatalf("opening entries %d: %v", openings, err)
	}
	if reply := f.apply(t, "existing", charge(1)); reply.Text != "original reply" {
		t.Fatalf("lost original receipt: %+v", reply)
	}
}

func TestLegacyDatesNewDebtAfterTimestampMigration(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	primary := filepath.Join(dir, "legacy.db")
	db, err := sql.Open("sqlite", primary)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(schema.Bootstrap + "ALTER TABLE debts DROP COLUMN inserted_at"); err != nil {
		t.Fatal(err)
	}
	f := openTrial(t, primary, filepath.Join(dir, "ghost.db"))

	// Act
	f.apply(t, "new-charge", charge(250))

	// Assert
	var inserted, updated int64
	if err := db.QueryRow("SELECT inserted_at, updated_at FROM debts").Scan(&inserted, &updated); err != nil {
		t.Fatal(err)
	}
	if inserted <= 0 || inserted != updated {
		t.Fatalf("new debt has inserted_at=%d, updated_at=%d", inserted, updated)
	}
}

func TestRestartReplaysLegacyCommitOrderOnce(t *testing.T) {
	// Arrange
	first := newTrial(t)
	first.apply(t, "charge", charge(2000))
	first.apply(t, "reset", parser.Command{Kind: parser.KindReset, Target: "B"})
	first.apply(t, "later", charge(300))
	first.apply(t, "later", charge(300))
	first.apply(t, "", parser.Command{Kind: parser.KindPaySet, PayMethod: "bank", PayValue: "sample"})
	first.apply(t, "status", parser.Command{Kind: parser.KindSummary})
	ghost := first.ledger(t)
	d, ok, err := first.Legacy.Next(t.Context())
	if err != nil || !ok {
		t.Fatalf("missing command: %v", err)
	}
	if err := ghost.Replay(t.Context(), d); err != nil {
		t.Fatal(err)
	} // Simulate exit before acknowledgement.
	if err := first.Legacy.Close(); err != nil {
		t.Fatal(err)
	}

	// Act
	restarted := openTrial(t, first.primary, first.ghost)
	if err := restarted.drain(t.Context(), restarted.ledger(t)); err != nil {
		t.Fatal(err)
	}

	// Assert
	assertReplaySequence(t, restarted.ghost, 5)
	var entries, balance int
	row := connect(t, first.ghost).QueryRow(`SELECT
		(SELECT count(*) FROM ledger_entries), (SELECT amount_minor FROM debts)`)
	err = row.Scan(&entries, &balance)
	if err != nil || entries != 3 || balance != 300 {
		t.Fatalf("entries=%d balance=%d: %v", entries, balance, err)
	}
}

func TestUpgradePreservesPendingReplay(t *testing.T) {
	// Arrange
	f := newTrial(t)
	ghost := f.ledger(t)
	f.apply(t, "already-replayed", charge(100))
	if err := f.drain(t.Context(), ghost); err != nil {
		t.Fatal(err)
	}
	f.apply(t, "pending", charge(200))
	if err := f.Legacy.Close(); err != nil {
		t.Fatal(err)
	}
	addOldComparisonColumns(t, f)

	// Act
	restarted := openTrial(t, f.primary, f.ghost)
	restarted.apply(t, "after-upgrade", charge(400))
	if err := restarted.drain(t.Context(), restarted.ledger(t)); err != nil {
		t.Fatal(err)
	}

	// Assert
	assertReplaySequence(t, f.ghost, 3)
	var balance int64
	if err := connect(t, f.ghost).QueryRow("SELECT amount_minor FROM debts").Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 700 {
		t.Fatalf("balance after upgrade = %d, want 700", balance)
	}
}

func addOldComparisonColumns(t *testing.T, f trialFixture) {
	t.Helper()
	liveDB, ghostDB := connect(t, f.primary), connect(t, f.ghost)
	for _, db := range []*sql.DB{liveDB, ghostDB} {
		_, err := db.Exec("ALTER TABLE shadow_trial ADD COLUMN mismatches INTEGER NOT NULL DEFAULT 0")
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := liveDB.Exec("ALTER TABLE shadow_queue ADD COLUMN expected BLOB NOT NULL DEFAULT X'00'")
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeDropsComparisonColumns(t *testing.T) {
	// Arrange
	f := newTrial(t)
	f.ledger(t)
	addOldComparisonColumns(t, f)

	// Act
	restarted := openTrial(t, f.primary, f.ghost)
	restarted.ledger(t)

	// Assert
	for _, db := range []*sql.DB{connect(t, f.primary), connect(t, f.ghost)} {
		var obsoleteColumns int
		err := db.QueryRow(`SELECT
			(SELECT count(*) FROM pragma_table_info('shadow_trial') WHERE name='mismatches') +
			(SELECT count(*) FROM pragma_table_info('shadow_queue') WHERE name='expected')`).Scan(&obsoleteColumns)
		if err != nil || obsoleteColumns != 0 {
			t.Fatalf("obsolete columns after upgrade = %d, err = %v", obsoleteColumns, err)
		}
	}
}

func TestGhostFailureDoesNotRollBackLegacy(t *testing.T) {
	// Arrange
	f := newTrial(t)
	ghost := f.ledger(t)
	db := connect(t, f.ghost)
	if _, err := db.Exec(`CREATE TRIGGER fail_post BEFORE INSERT ON ledger_entries
		BEGIN SELECT RAISE(ABORT,'unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	f.apply(t, "first", charge(200))

	// Act
	replayErr := f.drain(t.Context(), ghost)
	reply := f.apply(t, "next", charge(300))

	// Assert
	if replayErr == nil || reply.Text != "<@B> now owes you 5.00 PLN." {
		t.Fatalf("replay error=%v, legacy reply=%+v", replayErr, reply)
	}
	assertReplaySequence(t, f.primary, 2)
	assertReplaySequence(t, f.ghost, 0)
}

func TestDeclinedCommandsAdvanceGhostCheckpoint(t *testing.T) {
	// Arrange
	f := newTrial(t)
	f.apply(t, "maximum", charge(math.MaxInt64))
	_, err := f.Legacy.ApplyOnce(t.Context(), "overflow", "A", charge(1))
	if !errors.Is(err, engine.ErrBalanceOverflow) {
		t.Fatal(err)
	}
	f.apply(t, "missing", parser.Command{Kind: parser.KindPaySetDefault, PayMethod: "bank"})

	// Act
	err = f.drain(t.Context(), f.ledger(t))

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	assertReplaySequence(t, f.ghost, 3)
}

func TestLegacyRedeliveryKeepsOriginalPaymentDecline(t *testing.T) {
	// Arrange
	f := newTrial(t)
	selectBank := parser.Command{Kind: parser.KindPaySetDefault, PayMethod: "bank"}
	first := f.apply(t, "select-bank", selectBank)
	if !first.Declined {
		t.Fatal("selected a payment method before it was saved")
	}
	f.apply(t, "save-bank", parser.Command{Kind: parser.KindPaySet, PayMethod: "bank", PayValue: "account"})

	// Act
	retry := f.apply(t, "select-bank", selectBank)

	// Assert
	if retry != first {
		t.Fatalf("redelivery returned %+v, want original reply %+v", retry, first)
	}
	assertReplaySequence(t, f.primary, 2)
}

func TestMissingGhostIsNeverReseededFromLaterBalances(t *testing.T) {
	// Arrange
	f := newTrial(t)
	f.apply(t, "first", charge(200))
	if err := os.Rename(f.ghost, f.ghost+".saved"); err != nil {
		t.Fatal(err)
	}

	// Act
	reopened := openTrial(t, f.primary, f.ghost)
	reopened.apply(t, "second", charge(100))
	_, err := reopened.openLedger(context.Background())

	// Assert
	if err == nil {
		t.Fatal("opened a missing ghost")
	}
	if _, err := os.Stat(f.ghost); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing ghost was recreated: %v", err)
	}
}

func TestUnqueuedCommandRollsBackLegacy(t *testing.T) {
	// Arrange
	f := newTrial(t)
	db := connect(t, f.primary)
	if _, err := db.Exec(`CREATE TRIGGER fail_queue BEFORE INSERT ON shadow_queue
		BEGIN SELECT RAISE(ABORT,'queue unavailable'); END`); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err := f.Legacy.ApplyOnce(t.Context(), "charge", "A", charge(200))

	// Assert
	if err == nil {
		t.Fatal("command succeeded without a queue entry")
	}
	var debts, receipts, sequence int
	row := db.QueryRow(`SELECT (SELECT count(*) FROM debts), (SELECT count(*) FROM applied_commands), sequence
		FROM shadow_trial`)
	err = row.Scan(&debts, &receipts, &sequence)
	if err != nil || debts != 0 || receipts != 0 || sequence != 0 {
		t.Fatalf("debts=%d receipts=%d sequence=%d: %v", debts, receipts, sequence, err)
	}
}

func TestTrialRejectsAnotherTrialsGhost(t *testing.T) {
	// Arrange
	first, second := newTrial(t), newTrial(t)
	reconfigured := openTrial(t, first.primary, second.ghost)

	// Act
	_, err := reconfigured.openLedger(t.Context())

	// Assert
	if err == nil {
		t.Fatal("accepted a ghost from a different trial")
	}
}

func TestConcurrentWorkersApplyEachCommandOnce(t *testing.T) {
	// Arrange
	f := newTrial(t)
	const commands = 24
	group := new(sync.WaitGroup)
	errs := make(chan error, commands)
	for i := range commands {
		group.Go(func() {
			_, err := f.Legacy.ApplyOnce(t.Context(), strconv.Itoa(i), "A", charge(1))
			if err != nil {
				errs <- err
			}
		})
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	one, two := f.ledger(t), f.ledger(t)
	results := make(chan error, 2)

	// Act
	for _, g := range []*ledger.Ledger{one, two} {
		group.Go(func() { results <- f.drain(t.Context(), g) })
	}
	group.Wait()

	// Assert
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertReplaySequence(t, f.ghost, commands)
	var balance, entries int
	row := connect(t, f.ghost).QueryRow("SELECT (SELECT amount_minor FROM debts), count(*) FROM ledger_entries")
	err := row.Scan(&balance, &entries)
	if err != nil || balance != commands || entries != commands {
		t.Fatalf("balance=%d entries=%d: %v", balance, entries, err)
	}
}

type panickingLedger struct{}

func (panickingLedger) Replay(context.Context, engine.Delivery) error { panic("ledger bug") }

func TestLedgerPanicLeavesCommandQueued(t *testing.T) {
	// Arrange
	f := newTrial(t)
	f.apply(t, "charge", charge(100))

	// Act
	replayErr := f.drain(t.Context(), panickingLedger{})

	// Assert
	if replayErr == nil {
		t.Fatal("expected a ledger failure")
	}
	d, ok, err := f.Legacy.Next(t.Context())
	if err != nil || !ok || d.ID != "charge" {
		t.Fatalf("queued command %+v, err %v", d, err)
	}
}
