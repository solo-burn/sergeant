package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"sergeant/internal/database/schema"
	"sergeant/internal/engine"
)

func openTemp(t *testing.T) (*SQLite, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func (s *SQLite) postDelta(ctx context.Context, creditor, debtor, currency string, delta int64) error {
	_, err := s.WriteCommand(ctx, "", func(tx *Transaction) (string, error) {
		_, err := tx.AddDeltaWithSimplifiedDebt(ctx, creditor, debtor, currency, delta)
		return "", err
	})
	return err
}

func openStoreWithDebts(t *testing.T) *SQLite {
	t.Helper()
	s, _ := openTemp(t)
	for _, debt := range []Debt{
		{Creditor: "A", Debtor: "B", Currency: "PLN", AmountMinor: 2000},
		{Creditor: "A", Debtor: "B", Currency: "EUR", AmountMinor: 500},
		{Creditor: "A", Debtor: "C", Currency: "PLN", AmountMinor: 100},
		{Creditor: "X", Debtor: "B", Currency: "PLN", AmountMinor: 9999},
	} {
		err := s.postDelta(t.Context(), debt.Creditor, debt.Debtor, debt.Currency, debt.AmountMinor)
		if err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestSQLiteListDebts(t *testing.T) {
	for _, tc := range []struct {
		name string
		list func(*SQLite, context.Context, string) ([]Debt, error)
		user string
		want []Debt
	}{
		{"by creditor", (*SQLite).ListByCreditor, "A", []Debt{
			{Creditor: "A", Debtor: "B", Currency: "EUR", AmountMinor: 500},
			{Creditor: "A", Debtor: "B", Currency: "PLN", AmountMinor: 2000},
			{Creditor: "A", Debtor: "C", Currency: "PLN", AmountMinor: 100},
		}},
		{"by debtor", (*SQLite).ListByDebtor, "B", []Debt{
			{Creditor: "A", Debtor: "B", Currency: "EUR", AmountMinor: 500},
			{Creditor: "A", Debtor: "B", Currency: "PLN", AmountMinor: 2000},
			{Creditor: "X", Debtor: "B", Currency: "PLN", AmountMinor: 9999},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			s := openStoreWithDebts(t)

			// Act
			got, err := tc.list(s, t.Context(), tc.user)

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			for i, debt := range got {
				if debt.InsertedAt == 0 {
					t.Errorf("missing insertion time for %+v", debt)
				}
				got[i].InsertedAt = 0
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("debts = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSQLiteResetPairCurrency(t *testing.T) {
	// Arrange
	s := openStoreWithDebts(t)

	// Act
	err := s.ResetPairCurrency(t.Context(), "A", "B", "EUR")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListPair(t.Context(), "A", "B")
	if err != nil || len(rows) != 1 || rows[0].Currency != "PLN" {
		t.Fatalf("after reset EUR: %v, %v", rows, err)
	}
}

func TestSQLiteResetPair(t *testing.T) {
	// Arrange
	s := openStoreWithDebts(t)

	// Act
	err := s.ResetPair(t.Context(), "A", "B")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListPair(t.Context(), "A", "B")
	if err != nil || len(rows) != 0 {
		t.Fatalf("after reset all: %v, %v", rows, err)
	}
}

func TestSQLitePaymentMethods(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	if pms, _ := s.ListPaymentMethods(ctx, "U1"); len(pms) != 0 {
		t.Fatalf("expected empty, got %v", pms)
	}

	if err := s.SetPaymentMethod(ctx, "U1", "bank", "PL61 1090 0000"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPaymentMethod(ctx, "U1", "blik", "555 555 555"); err != nil {
		t.Fatal(err)
	}
	pms, err := s.ListPaymentMethods(ctx, "U1")
	if err != nil {
		t.Fatal(err)
	}
	want := []PaymentMethod{
		{UserID: "U1", Method: "bank", Value: "PL61 1090 0000"},
		{UserID: "U1", Method: "blik", Value: "555 555 555"},
	}
	if len(pms) != len(want) {
		t.Fatalf("got %v, want %v", pms, want)
	}
	for i := range pms {
		if pms[i].InsertedAt == 0 {
			t.Errorf("row %d: InsertedAt was not set", i)
		}
		pms[i].InsertedAt = 0
		if pms[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, pms[i], want[i])
		}
	}

	if err := s.SetPaymentMethod(ctx, "U1", "bank", "PL99 9999 9999"); err != nil {
		t.Fatal(err)
	}
	pms, _ = s.ListPaymentMethods(ctx, "U1")
	if pms[0].Value != "PL99 9999 9999" {
		t.Fatalf("update did not overwrite, got %v", pms[0])
	}

	if err := s.RemovePaymentMethod(ctx, "U1", "blik"); err != nil {
		t.Fatal(err)
	}
	pms, _ = s.ListPaymentMethods(ctx, "U1")
	if len(pms) != 1 || pms[0].Method != "bank" {
		t.Fatalf("after remove blik: %v", pms)
	}

	_ = s.SetPaymentMethod(ctx, "U2", "revolut", "@kamil")
	if err := s.ClearPaymentMethods(ctx, "U1"); err != nil {
		t.Fatal(err)
	}
	if pms, _ := s.ListPaymentMethods(ctx, "U1"); len(pms) != 0 {
		t.Fatalf("U1 not cleared: %v", pms)
	}
	if pms, _ := s.ListPaymentMethods(ctx, "U2"); len(pms) != 1 {
		t.Fatalf("U2 should be untouched: %v", pms)
	}
}

func TestSQLiteDefaultPaymentMethod(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	err := s.SetDefaultPaymentMethod(ctx, "U1", "bank")
	if !errors.Is(err, engine.ErrUnknownMethod) {
		t.Fatalf("expected unknown payment method, got %v", err)
	}

	if err := s.SetPaymentMethod(ctx, "U1", "bank", "PL61"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPaymentMethod(ctx, "U1", "blik", "555"); err != nil {
		t.Fatal(err)
	}

	pms, _ := s.ListPaymentMethods(ctx, "U1")
	for _, pm := range pms {
		if pm.IsDefault {
			t.Fatalf("expected no defaults, got %+v", pm)
		}
	}

	if err := s.SetDefaultPaymentMethod(ctx, "U1", "blik"); err != nil {
		t.Fatal(err)
	}
	pms, _ = s.ListPaymentMethods(ctx, "U1")
	if len(pms) != 2 || pms[0].Method != "blik" || !pms[0].IsDefault || pms[1].IsDefault {
		t.Fatalf("after set-default blik: %+v", pms)
	}

	if err := s.SetDefaultPaymentMethod(ctx, "U1", "bank"); err != nil {
		t.Fatal(err)
	}
	pms, _ = s.ListPaymentMethods(ctx, "U1")
	if len(pms) != 2 || pms[0].Method != "bank" || !pms[0].IsDefault || pms[1].IsDefault {
		t.Fatalf("after set-default bank: %+v", pms)
	}

	if err := s.SetPaymentMethod(ctx, "U1", "bank", "PL99"); err != nil {
		t.Fatal(err)
	}
	pms, _ = s.ListPaymentMethods(ctx, "U1")
	if pms[0].Method != "bank" || pms[0].Value != "PL99" || !pms[0].IsDefault {
		t.Fatalf("update should preserve default: %+v", pms)
	}

	if err := s.RemovePaymentMethod(ctx, "U1", "bank"); err != nil {
		t.Fatal(err)
	}
	pms, _ = s.ListPaymentMethods(ctx, "U1")
	if len(pms) != 1 || pms[0].IsDefault {
		t.Fatalf("removing default should leave no default: %+v", pms)
	}
}

func TestSQLiteDefaultPaymentMethodLookup(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	if _, ok, err := s.DefaultPaymentMethod(ctx, "U1"); err != nil || ok {
		t.Fatalf("expected (false, nil) for missing user, got ok=%v err=%v", ok, err)
	}

	if err := s.SetPaymentMethod(ctx, "U1", "bank", "PL61"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPaymentMethod(ctx, "U1", "blik", "555"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.DefaultPaymentMethod(ctx, "U1"); err != nil || ok {
		t.Fatalf("no default set: ok=%v err=%v", ok, err)
	}

	if err := s.SetDefaultPaymentMethod(ctx, "U1", "blik"); err != nil {
		t.Fatal(err)
	}
	pm, ok, err := s.DefaultPaymentMethod(ctx, "U1")
	if err != nil || !ok {
		t.Fatalf("expected blik as default, got ok=%v err=%v", ok, err)
	}
	if pm.Method != "blik" || pm.Value != "555" || !pm.IsDefault {
		t.Fatalf("unexpected default row: %+v", pm)
	}
}

func TestSQLiteMigrateFromV0(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	rawDB, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.Exec(schema.Bootstrap); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.Exec(
		`INSERT INTO payment_methods(user_id, method, value, updated_at)
		 VALUES ('U1', 'bank', 'PL61', unixepoch())`); err != nil {
		t.Fatal(err)
	}
	_ = rawDB.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open after legacy seed: %v", err)
	}
	defer func() { _ = s.Close() }()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schema.JournalVersion {
		t.Fatalf("user_version = %d, want %d", version, schema.JournalVersion)
	}

	pms, err := s.ListPaymentMethods(ctx, "U1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pms) != 1 || pms[0].Value != "PL61" || pms[0].IsDefault {
		t.Fatalf("after migrate: %+v", pms)
	}
	if err := s.SetDefaultPaymentMethod(ctx, "U1", "bank"); err != nil {
		t.Fatalf("set-default after migrate: %v", err)
	}
}

func TestSQLiteMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "current.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPaymentMethod(ctx, "U1", "bank", "PL61"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDefaultPaymentMethod(ctx, "U1", "bank"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()

	var version int
	if err := s2.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schema.JournalVersion {
		t.Fatalf("user_version = %d, want %d", version, schema.JournalVersion)
	}
	pms, _ := s2.ListPaymentMethods(ctx, "U1")
	if len(pms) != 1 || !pms[0].IsDefault {
		t.Fatalf("default flag lost across reopen: %+v", pms)
	}
}

func TestSQLiteMigrateRejectsFutureSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	rawDB, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.Exec(schema.Bootstrap); err != nil {
		t.Fatal(err)
	}
	futureVersion := fmt.Sprintf("PRAGMA user_version = %d", schema.JournalVersion+5)
	if _, err := rawDB.Exec(futureVersion); err != nil {
		t.Fatal(err)
	}
	_ = rawDB.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("expected Open to reject a future schema version")
	}
}

func TestSQLitePersistsAcrossReopen(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), "persist.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.postDelta(t.Context(), "A", "B", "PLN", 1234); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Act
	s2, err := Open(path)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	rows, err := s2.ListPair(t.Context(), "A", "B")
	if err != nil || len(rows) != 1 || rows[0].AmountMinor != 1234 {
		t.Fatalf("balance after reopen: %v, %v", rows, err)
	}
}

func TestConcurrentAdjustments(t *testing.T) {
	// Arrange
	s, _ := openTemp(t)
	ctx := t.Context()

	const goroutines = 50
	const perGoroutine = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Act
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if err := s.postDelta(ctx, "A", "B", "PLN", 1); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Assert
	rows, err := s.ListPair(ctx, "A", "B")
	if err != nil {
		t.Fatal(err)
	}
	want := int64(goroutines * perGoroutine)
	if len(rows) != 1 || rows[0].AmountMinor != want {
		t.Fatalf("final balance = %v, want single row of %d", rows, want)
	}
}

func TestConcurrentAdjustmentsMixedSigns(t *testing.T) {
	// Arrange
	s, _ := openTemp(t)
	ctx := t.Context()

	if err := s.postDelta(ctx, "A", "B", "PLN", 100000); err != nil {
		t.Fatal(err)
	}

	const goroutines = 40
	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Act
	for i := 0; i < goroutines; i++ {
		delta := int64(1)
		if i%2 == 1 {
			delta = -1
		}
		go func(d int64) {
			defer wg.Done()
			if err := s.postDelta(ctx, "A", "B", "PLN", d); err != nil {
				t.Error(err)
			}
		}(delta)
	}
	wg.Wait()

	// Assert
	rows, err := s.ListPair(ctx, "A", "B")
	if err != nil || len(rows) != 1 || rows[0].AmountMinor != 100000 {
		t.Fatalf("final balance = %v, want 100000 (err %v)", rows, err)
	}
}

func TestUpgradeConsolidatesLegacyOpposingBalances(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(schema.Bootstrap); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO debts(creditor_id, debtor_id, currency, amount_minor, updated_at) VALUES
		('A', 'B', 'EUR', 20, 1), ('B', 'A', 'EUR', 12, 1),
		('A', 'B', 'PLN',  1, 1), ('B', 'A', 'PLN', 10, 1),
		('A', 'C', 'EUR',  9, 1), ('C', 'A', 'EUR',  9, 1),
		('C', 'B', 'GBP', 25, 1)`); err != nil {
		t.Fatal(err)
	}
	type balanceKey struct{ creditor, debtor, currency string }
	want := map[balanceKey]int64{
		{"A", "B", "EUR"}: 8,
		{"B", "A", "PLN"}: 9,
		{"C", "B", "GBP"}: 25,
	}

	// Act
	s, err := Open(path)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	got := map[balanceKey]int64{}
	for _, user := range []string{"A", "B", "C"} {
		rows, err := s.ListByCreditor(t.Context(), user)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			got[balanceKey{r.Creditor, r.Debtor, r.Currency}] = r.AmountMinor
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("upgraded balances %v, want %v", got, want)
	}
}

func TestConcurrentOpenMigratesOnce(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), "shared.db")
	const count = 8
	start := make(chan struct{})
	results := make(chan error, count)
	var group sync.WaitGroup
	for range count {
		group.Go(func() {
			<-start
			s, err := Open(path)
			if err == nil {
				err = s.Close()
			}
			results <- err
		})
	}

	// Act
	close(start)
	group.Wait()

	// Assert
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("opening shared database: %v", err)
		}
	}
}

func TestReceiptFailureRollsBackDebt(t *testing.T) {
	// Arrange
	st, _ := openTemp(t)
	if _, err := st.db.Exec(`CREATE TRIGGER receipt_failure BEFORE INSERT ON applied_commands
		BEGIN SELECT RAISE(ABORT, 'receipt write failed'); END`); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err := st.WriteCommand(t.Context(), "delivery", func(tx *Transaction) (string, error) {
		_, err := tx.AddDeltaWithSimplifiedDebt(t.Context(), "A", "B", "PLN", 500)
		return "saved", err
	})

	// Assert
	if err == nil {
		t.Fatal("command succeeded without a receipt")
	}
	rows, readErr := st.ListPair(t.Context(), "A", "B")
	if readErr != nil || len(rows) != 0 {
		t.Fatalf("debt after failed commit: %v, %v", rows, readErr)
	}
	var entries int
	err = st.db.QueryRow("SELECT count(*) FROM ledger_entries").Scan(&entries)
	if err != nil || entries != 0 {
		t.Fatalf("entries after failed commit: %d, %v", entries, err)
	}
}

func TestBalanceReportUsesOneSnapshot(t *testing.T) {
	// Arrange
	st, _ := openTemp(t)
	if err := st.postDelta(t.Context(), "A", "B", "PLN", 500); err != nil {
		t.Fatal(err)
	}
	var owed, owe []Debt

	// Act
	_, err := st.ReadSnapshot(t.Context(), func(tx *Transaction) (string, error) {
		var err error
		owed, err = tx.ListByCreditor(t.Context(), "A")
		if err != nil {
			return "", err
		}
		if err := st.postDelta(t.Context(), "A", "B", "PLN", -700); err != nil {
			return "", err
		}
		owe, err = tx.ListByDebtor(t.Context(), "A")
		return "", err
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(owed) != 1 || owed[0].AmountMinor != 500 || len(owe) != 0 {
		t.Fatalf("one report mixed before and after balances: owed %v, owe %v", owed, owe)
	}
}

func TestDatabasePathWithQuestionMarkKeepsAccountsSeparate(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	first, err := Open(filepath.Join(dir, "team?one.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if err := first.SetPaymentMethod(t.Context(), "A", "bank", "first account"); err != nil {
		t.Fatal(err)
	}

	// Act
	second, err := Open(filepath.Join(dir, "team?two.db"))

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	got, err := second.ListPaymentMethods(t.Context(), "A")
	if err != nil || len(got) != 0 {
		t.Fatalf("new database contains the other account: %v, %v", got, err)
	}
}

func TestConcurrentDeliveriesAcrossDatabaseHandlesApplyOnce(t *testing.T) {
	// Arrange
	first, path := openTemp(t)
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	start := make(chan struct{})
	const deliveries = 16
	results := make(chan error, deliveries)
	apply := func(tx *Transaction) (string, error) {
		_, err := tx.AddDeltaWithSimplifiedDebt(t.Context(), "A", "B", "PLN", 200)
		return "saved", err
	}
	var group sync.WaitGroup
	for i := range deliveries {
		db := first
		if i%2 == 1 {
			db = second
		}
		group.Go(func() {
			<-start
			_, err := db.WriteCommand(t.Context(), "same-event", apply)
			results <- err
		})
	}

	// Act
	close(start)
	group.Wait()

	// Assert
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := first.ListPair(t.Context(), "A", "B")
	if err != nil || len(rows) != 1 || rows[0].AmountMinor != 200 {
		t.Fatalf("balance after concurrent redelivery: %v, %v", rows, err)
	}
}

func TestJournalRejectsChangesToPostedEntries(t *testing.T) {
	for _, tc := range []struct{ name, statement string }{
		{"update", "UPDATE ledger_entries SET amount_minor = 200"},
		{"delete", "DELETE FROM ledger_entries"},
		{"replace", "INSERT OR REPLACE INTO ledger_entries SELECT * FROM ledger_entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			s, _ := openTemp(t)
			err := s.postDelta(t.Context(), "A", "B", "PLN", 100)
			if err != nil {
				t.Fatal(err)
			}

			// Act
			_, err = s.db.Exec(tc.statement)

			// Assert
			if err == nil {
				t.Fatal("changed an existing journal entry")
			}
			var amount int64
			err = s.db.QueryRow("SELECT amount_minor FROM ledger_entries").Scan(&amount)
			if err != nil || amount != 100 {
				t.Fatalf("original posting: amount=%d, err=%v", amount, err)
			}
		})
	}
}

func TestOpeningBalancesFromUnversionedDump(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), "restored.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	_, err = raw.Exec(schema.Bootstrap + `
		ALTER TABLE payment_methods ADD COLUMN is_default INTEGER NOT NULL DEFAULT 0;
		INSERT INTO debts VALUES('A','B','PLN',3956,10,20);
		INSERT INTO payment_methods VALUES('A','custom','sample',11,23,1);`)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	s, err := Open(path)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var opening, balance, method int
	err = raw.QueryRow(`SELECT
		(SELECT count(*) FROM ledger_entries WHERE kind='opening'
		 AND actor_id IS NULL AND command_id IS NULL
		 AND creditor_id='A' AND debtor_id='B' AND currency='PLN'
		 AND amount_minor=3956 AND recorded_at=20),
		(SELECT count(*) FROM debts WHERE amount_minor=3956 AND inserted_at=10 AND updated_at=20),
		(SELECT count(*) FROM payment_methods WHERE user_id='A' AND method='custom' AND value='sample'
		 AND inserted_at=11 AND updated_at=23 AND is_default=1)`).Scan(&opening, &balance, &method)
	if err != nil || opening != 1 || balance != 1 || method != 1 {
		t.Fatalf("restored opening=%d balance=%d payment=%d: %v", opening, balance, method, err)
	}
}

func TestReopeningUnversionedJournalKeepsHistory(t *testing.T) {
	// Arrange
	s, path := openTemp(t)
	if err := s.postDelta(t.Context(), "A", "B", "PLN", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 0"); err != nil {
		t.Fatal(err)
	}

	// Act
	reopened, err := Open(path)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	var entries, openings int
	row := reopened.db.QueryRow(`
		SELECT count(*), count(*) FILTER (WHERE kind='opening') FROM ledger_entries`)
	err = row.Scan(&entries, &openings)
	if err != nil || entries != 1 || openings != 0 {
		t.Fatalf("entries=%d openings=%d: %v", entries, openings, err)
	}
}

func TestEarliestSchemaUsesKnownDatesForOpeningBalances(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), "early.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	_, err = raw.Exec(schema.Bootstrap + `
		ALTER TABLE debts DROP COLUMN inserted_at;
		ALTER TABLE payment_methods DROP COLUMN inserted_at;
		INSERT INTO debts VALUES ('A','B','EUR',750,25);
		INSERT INTO payment_methods VALUES ('A','bank','sample',30);`)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	s, err := Open(path)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var openedAt int64
	row := raw.QueryRow("SELECT recorded_at FROM ledger_entries WHERE kind='opening'")
	err = row.Scan(&openedAt)
	if err != nil || openedAt != 25 {
		t.Fatalf("opening date = %d, err=%v", openedAt, err)
	}
	methods, err := s.ListPaymentMethods(t.Context(), "A")
	if err != nil || len(methods) != 1 || methods[0].InsertedAt != 30 {
		t.Fatalf("legacy payment methods = %+v, err=%v", methods, err)
	}
}

func TestResetRollsBackEveryCurrencyOnPostingFailure(t *testing.T) {
	// Arrange
	s, _ := openTemp(t)
	for _, currency := range []string{"EUR", "PLN"} {
		if err := s.postDelta(t.Context(), "A", "B", currency, 500); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_one_reset BEFORE INSERT ON ledger_entries
		WHEN NEW.kind='reset' AND NEW.currency='PLN'
		BEGIN SELECT RAISE(ABORT,'posting failed'); END`); err != nil {
		t.Fatal(err)
	}

	// Act
	err := s.ResetPair(t.Context(), "A", "B")

	// Assert
	if err == nil {
		t.Fatal("reset succeeded despite a failed posting")
	}
	var balances, resets int
	row := s.db.QueryRow(`SELECT (SELECT count(*) FROM debts WHERE amount_minor=500),
		(SELECT count(*) FROM ledger_entries WHERE kind='reset')`)
	err = row.Scan(&balances, &resets)
	if err != nil || balances != 2 || resets != 0 {
		t.Fatalf("after failed reset: balances=%d reset entries=%d err=%v", balances, resets, err)
	}
}
