package ledger

import (
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"sergeant/internal/engine"
	"sergeant/internal/i18n"
	"sergeant/internal/ledger/store"
	"sergeant/internal/parser"
)

func newLedger(t *testing.T) *Ledger { return newLedgerLocale(t, "en") }

func newLedgerLocale(t *testing.T, locale string) *Ledger {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s, "PLN", i18n.New(locale))
}

func add(t *testing.T, l *Ledger, creditor, target string, sign int, minor int64, ccy string) engine.Reply {
	t.Helper()
	return applyCommand(t, l, creditor, parser.Command{
		Kind: parser.KindAdd, Target: target, Sign: sign, Minor: minor, Currency: ccy,
	})
}

func applyCommand(t *testing.T, l *Ledger, author string, cmd parser.Command) engine.Reply {
	t.Helper()
	r, err := l.Apply(t.Context(), author, cmd)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func assertReplyText(t *testing.T, reply engine.Reply, want string) {
	t.Helper()
	if reply.Text != want {
		t.Errorf("got %q, want %q", reply.Text, want)
	}
}

func TestLedger(t *testing.T) {
	t.Run("adjustments", testAdjustments)
	t.Run("resets", testResets)
	t.Run("status", testStatus)
	t.Run("net balances", testNetBalances)
	t.Run("summary", testSummary)
	t.Run("payment methods", testPaymentMethods)
}

func testAdjustments(t *testing.T) {
	t.Run("add accumulates and uses default currency", func(t *testing.T) {
		// Arrange
		l := newLedger(t)
		add(t, l, "A", "B", 1, 2000, "")

		// Act
		r := add(t, l, "A", "B", 1, 500, "")

		// Assert
		assertReplyText(t, r, "<@B> now owes you 25.00 PLN.")
	})

	t.Run("overpayment reverses the debt", func(t *testing.T) {
		// Arrange
		l := newLedger(t)
		add(t, l, "A", "B", 1, 1000, "PLN")

		// Act
		r := add(t, l, "A", "B", -1, 9999, "PLN")

		// Assert
		assertReplyText(t, r, "You now owe <@B> 89.99 PLN.")
	})

	t.Run("self-target rejected", func(t *testing.T) {
		// Arrange
		l := newLedger(t)

		// Act
		_, err := l.Apply(t.Context(), "A", parser.Command{
			Kind: parser.KindAdd, Target: "A", Sign: 1, Minor: 100, Currency: "PLN",
		})

		// Assert
		if !errors.Is(err, engine.ErrSelfTarget) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("Polish adjustment reply", func(t *testing.T) {
		// Arrange
		l := newLedgerLocale(t, "pl")

		// Act
		reply := add(t, l, "A", "B", 1, 2000, "PLN")

		// Assert
		assertReplyText(t, reply, "<@B> jest tobie winny 20.00 PLN.")
	})
}

func testResets(t *testing.T) {
	for _, tc := range []struct {
		name, currency string
		wantCurrencies []string
	}{
		{name: "reset one currency", currency: "EUR", wantCurrencies: []string{"PLN"}},
		{name: "reset every currency"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			l := newLedger(t)
			add(t, l, "A", "B", 1, 100, "PLN")
			add(t, l, "A", "B", 1, 100, "EUR")

			// Act
			applyCommand(t, l, "A", parser.Command{Kind: parser.KindReset, Target: "B", Currency: tc.currency})

			// Assert
			rows, err := l.store.ListPair(t.Context(), "A", "B")
			if err != nil {
				t.Fatal(err)
			}
			var currencies []string
			for _, row := range rows {
				currencies = append(currencies, row.Currency)
			}
			if !slices.Equal(currencies, tc.wantCurrencies) {
				t.Fatalf("remaining currencies = %v, want %v", currencies, tc.wantCurrencies)
			}
		})
	}

	t.Run("reset clears both directions", func(t *testing.T) {
		// Arrange
		l := newLedger(t)
		add(t, l, "A", "B", 1, 2000, "PLN")
		add(t, l, "B", "A", 1, 1000, "PLN")

		before := applyCommand(t, l, "A", parser.Command{Kind: parser.KindStatusFor, Target: "B"})
		assertReplyText(t, before, "<@B> owes you 10.00 PLN.")

		// Act
		applyCommand(t, l, "A", parser.Command{Kind: parser.KindReset, Target: "B"})

		// Assert
		fromA := applyCommand(t, l, "A", parser.Command{Kind: parser.KindStatusFor, Target: "B"})
		assertReplyText(t, fromA, "<@B> owes you nothing.")

		fromB := applyCommand(t, l, "B", parser.Command{Kind: parser.KindStatusFor, Target: "A"})
		assertReplyText(t, fromB, "<@A> owes you nothing.")
	})
}

func testStatus(t *testing.T) {
	t.Run("status for a person with no debt", func(t *testing.T) {
		// Arrange
		l := newLedger(t)

		// Act
		reply := applyCommand(t, l, "A", parser.Command{Kind: parser.KindStatusFor, Target: "B"})

		// Assert
		assertReplyText(t, reply, "<@B> owes you nothing.")
	})

	t.Run("status lists the amounts in currency order", func(t *testing.T) {
		// Arrange
		l := newLedger(t)
		add(t, l, "A", "B", 1, 2000, "PLN")
		add(t, l, "A", "B", 1, 500, "EUR")

		// Act
		reply := applyCommand(t, l, "A", parser.Command{Kind: parser.KindStatusFor, Target: "B"})

		// Assert
		assertReplyText(t, reply, "<@B> owes you 5.00 EUR, 20.00 PLN.")
	})

	t.Run("Polish status shows the outstanding debt", func(t *testing.T) {
		// Arrange
		l := newLedgerLocale(t, "pl")
		add(t, l, "A", "B", 1, 2000, "PLN")

		// Act
		reply := applyCommand(t, l, "A", parser.Command{Kind: parser.KindStatusAll})

		// Assert
		assertReplyText(t, reply, "Mają u Ciebie dług:\n- <@B> - 20.00 PLN")
	})

	t.Run("Polish status after settlement", func(t *testing.T) {
		// Arrange
		l := newLedgerLocale(t, "pl")
		add(t, l, "A", "B", 1, 2000, "PLN")
		applyCommand(t, l, "A", parser.Command{Kind: parser.KindReset, Target: "B"})

		// Act
		reply := applyCommand(t, l, "A", parser.Command{Kind: parser.KindStatusAll})

		// Assert
		assertReplyText(t, reply, "Nikt nie ma u Ciebie długu i Ty też nie.")
	})

	for _, tc := range []struct{ name, author, want string }{
		{"status lists only the viewer's debts", "B", "You owe:\n- <@A> - 5.00 EUR, 20.00 PLN"},
		{"status groups balances by debtor", "A", "Owed to you:\n- <@B> - 5.00 EUR, 20.00 PLN\n- <@C> - 1.00 PLN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			l := newLedger(t)
			add(t, l, "A", "B", 1, 2000, "PLN")
			add(t, l, "A", "B", 1, 500, "EUR")
			add(t, l, "A", "C", 1, 100, "PLN")

			// Act
			reply := applyCommand(t, l, tc.author, parser.Command{Kind: parser.KindStatusAll})

			// Assert
			assertReplyText(t, reply, tc.want)
		})
	}

	t.Run("status-all shows both sections when both directions exist", func(t *testing.T) {
		// Arrange
		l := newLedger(t)
		add(t, l, "A", "B", 1, 2000, "PLN") // B owes A
		add(t, l, "C", "A", 1, 1500, "EUR") // A owes C
		add(t, l, "D", "A", 1, 700, "PLN")  // A owes D

		// Act
		aView := applyCommand(t, l, "A", parser.Command{Kind: parser.KindStatusAll})

		// Assert
		want := "Owed to you:\n- <@B> - 20.00 PLN\nYou owe:\n- <@C> - 15.00 EUR\n- <@D> - 7.00 PLN"
		assertReplyText(t, aView, want)
	})

	t.Run("status-all you-owe lines include creditor's default payment method", func(t *testing.T) {
		// Arrange
		l := newLedger(t)

		add(t, l, "C", "A", 1, 3600, "PLN") // A owes C 36.00 PLN
		add(t, l, "D", "A", 1, 1500, "EUR") // A owes D 15.00 EUR (D has no default)

		applyCommand(t, l, "C", parser.Command{
			Kind: parser.KindPaySet, PayMethod: "blik", PayValue: "555 555 555",
		})
		applyCommand(t, l, "C", parser.Command{Kind: parser.KindPaySetDefault, PayMethod: "blik"})

		// Act
		aView := applyCommand(t, l, "A", parser.Command{Kind: parser.KindStatusAll})

		// Assert
		want := "You owe:\n- <@C> - 36.00 PLN (`blik 555 555 555`)\n- <@D> - 15.00 EUR"
		assertReplyText(t, aView, want)
	})

	t.Run("status-all owed-to-you lines never include payment method", func(t *testing.T) {
		// Arrange
		l := newLedger(t)

		add(t, l, "A", "B", 1, 2000, "PLN") // B owes A

		applyCommand(t, l, "A", parser.Command{
			Kind: parser.KindPaySet, PayMethod: "blik", PayValue: "111",
		})
		applyCommand(t, l, "A", parser.Command{Kind: parser.KindPaySetDefault, PayMethod: "blik"})
		applyCommand(t, l, "B", parser.Command{
			Kind: parser.KindPaySet, PayMethod: "bank", PayValue: "PL61",
		})
		applyCommand(t, l, "B", parser.Command{Kind: parser.KindPaySetDefault, PayMethod: "bank"})

		// Act
		aView := applyCommand(t, l, "A", parser.Command{Kind: parser.KindStatusAll})

		// Assert
		want := "Owed to you:\n- <@B> - 20.00 PLN"
		assertReplyText(t, aView, want)
	})

	t.Run("status-all empty when no rows on either side", func(t *testing.T) {
		// Arrange
		l := newLedger(t)

		// Act
		r := applyCommand(t, l, "Z", parser.Command{Kind: parser.KindStatusAll})

		// Assert
		assertReplyText(t, r, "Nobody owes you and you owe nothing.")
	})
}

func testNetBalances(t *testing.T) {
	for _, tc := range []struct{ name, author, want string }{
		{"net debt from the debtor's side", "Adam", "You owe:\n- <@Kamil> - 15.00 PLN"},
		{"net debt from the creditor's side", "Kamil", "Owed to you:\n- <@Adam> - 15.00 PLN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			l := newLedger(t)
			add(t, l, "Adam", "Kamil", 1, 1500, "PLN")
			add(t, l, "Kamil", "Adam", 1, 3000, "PLN")

			// Act
			reply := applyCommand(t, l, tc.author, parser.Command{Kind: parser.KindStatusAll})

			// Assert
			assertReplyText(t, reply, tc.want)
		})
	}

	t.Run("net debt simplification - exact cancel shows empty", func(t *testing.T) {
		// Arrange
		l := newLedger(t)
		add(t, l, "A", "B", 1, 2000, "PLN")
		add(t, l, "B", "A", 1, 2000, "PLN")

		// Act
		r := applyCommand(t, l, "A", parser.Command{Kind: parser.KindStatusAll})

		// Assert
		assertReplyText(t, r, "Nobody owes you and you owe nothing.")
	})
}

func testSummary(t *testing.T) {
	t.Run("summary empty", func(t *testing.T) {
		// Arrange
		l := newLedger(t)

		// Act
		r := applyCommand(t, l, "A", parser.Command{Kind: parser.KindSummary})

		// Assert
		assertReplyText(t, r, "Nobody owes you anything.")
	})

	t.Run("summary with debts but no payment methods", func(t *testing.T) {
		// Arrange
		l := newLedger(t)
		add(t, l, "A", "B", 1, 2000, "PLN")
		add(t, l, "A", "C", 1, 3470, "PLN")

		// Act
		r := applyCommand(t, l, "A", parser.Command{Kind: parser.KindSummary})

		// Assert
		want := "Summary:\n- <@B> - 20.00 PLN\n- <@C> - 34.70 PLN"
		assertReplyText(t, r, want)
	})

	t.Run("summary with debts and payment methods", func(t *testing.T) {
		// Arrange
		l := newLedger(t)
		add(t, l, "A", "B", 1, 2300, "PLN")
		add(t, l, "A", "C", 1, 3400, "PLN")
		applyCommand(t, l, "A", parser.Command{
			Kind: parser.KindPaySet, PayMethod: "bank", PayValue: "PL61 1090 1234",
		})
		applyCommand(t, l, "A", parser.Command{
			Kind: parser.KindPaySet, PayMethod: "blik", PayValue: "555 555 555",
		})

		// Act
		r := applyCommand(t, l, "A", parser.Command{Kind: parser.KindSummary})

		// Assert
		want := "Summary:" +
			"\n- <@B> - 23.00 PLN" +
			"\n- <@C> - 34.00 PLN" +
			"\n\n:money_with_wings:" +
			"\n- `bank` - PL61 1090 1234" +
			"\n- `blik` - 555 555 555"
		assertReplyText(t, r, want)
	})

	t.Run("summary with payments including default", func(t *testing.T) {
		// Arrange
		l := newLedger(t)
		add(t, l, "A", "B", 1, 2300, "PLN")
		applyCommand(t, l, "A", parser.Command{
			Kind: parser.KindPaySet, PayMethod: "bank", PayValue: "PL61 1090 1234",
		})
		applyCommand(t, l, "A", parser.Command{
			Kind: parser.KindPaySet, PayMethod: "blik", PayValue: "555 555 555",
		})
		applyCommand(t, l, "A", parser.Command{Kind: parser.KindPaySetDefault, PayMethod: "blik"})

		// Act
		r := applyCommand(t, l, "A", parser.Command{Kind: parser.KindSummary})

		// Assert
		want := "Summary:" +
			"\n- <@B> - 23.00 PLN" +
			"\n\n:money_with_wings:" +
			"\n- `blik` - 555 555 555 (default)" +
			"\n- `bank` - PL61 1090 1234"
		assertReplyText(t, r, want)
	})

	t.Run("summary polish locale", func(t *testing.T) {
		// Arrange
		l := newLedgerLocale(t, "pl")
		add(t, l, "A", "B", 1, 2300, "PLN")

		// Act
		r := applyCommand(t, l, "A", parser.Command{Kind: parser.KindSummary})

		// Assert
		want := "Podsumowanie:\n- <@B> - 23.00 PLN"
		assertReplyText(t, r, want)
	})

	t.Run("summary polish empty", func(t *testing.T) {
		// Arrange
		l := newLedgerLocale(t, "pl")

		// Act
		r := applyCommand(t, l, "A", parser.Command{Kind: parser.KindSummary})

		// Assert
		assertReplyText(t, r, "Nikt nie ma u ciebie długu.")
	})

	t.Run("summary polish with pay methods", func(t *testing.T) {
		// Arrange
		l := newLedgerLocale(t, "pl")
		add(t, l, "A", "B", 1, 2300, "PLN")
		applyCommand(t, l, "A", parser.Command{
			Kind: parser.KindPaySet, PayMethod: "blik", PayValue: "555 555 555",
		})

		// Act
		r := applyCommand(t, l, "A", parser.Command{Kind: parser.KindSummary})

		// Assert
		want := "Podsumowanie:\n- <@B> - 23.00 PLN\n\n:money_with_wings:\n- `blik` - 555 555 555"
		assertReplyText(t, r, want)
	})
}

func testPaymentMethods(t *testing.T) {
	saveBank := parser.Command{Kind: parser.KindPaySet, PayMethod: "bank", PayValue: "PL61 1090"}
	saveBlik := parser.Command{Kind: parser.KindPaySet, PayMethod: "blik", PayValue: "555 555 555"}
	selectBlik := parser.Command{Kind: parser.KindPaySetDefault, PayMethod: "blik"}
	removeBlik := parser.Command{Kind: parser.KindPayRemove, PayMethod: "blik"}
	clearMethods := parser.Command{Kind: parser.KindPayClear}
	showOwn := parser.Command{Kind: parser.KindPayShowSelf}
	showA := parser.Command{Kind: parser.KindPayShowFor, Target: "A"}
	bothMethods := "<@A> :money_with_wings: \n- `bank` - PL61 1090\n- `blik` - 555 555 555"
	empty := "You haven't added any payment methods yet. " +
		"Try `@sergeant pay set bank PL61 ...` or `@sergeant pay set blik 555 555 555`."

	for _, tc := range []struct {
		name    string
		setup   []parser.Command
		author  string
		command parser.Command
		want    string
	}{
		{name: "empty list suggests setup", author: "A", command: showOwn, want: empty},
		{
			name: "owner can see both methods", setup: []parser.Command{saveBank, saveBlik},
			author: "A", command: showOwn, want: bothMethods,
		},
		{
			name: "another user can see both methods", setup: []parser.Command{saveBank, saveBlik},
			author: "B", command: showA, want: bothMethods,
		},
		{
			name: "removing one method keeps the other", setup: []parser.Command{saveBank, saveBlik, removeBlik},
			author: "A", command: showOwn, want: "<@A> :money_with_wings: \n- `bank` - PL61 1090",
		},
		{
			name: "clearing methods empties the list", setup: []parser.Command{saveBank, clearMethods},
			author: "A", command: showOwn, want: empty,
		},
		{
			name: "confirms the selected default", setup: []parser.Command{saveBank, saveBlik},
			author: "A", command: selectBlik, want: "Set `blik` as your default payment method.",
		},
		{
			name: "marks only the default and lists it first", setup: []parser.Command{saveBank, saveBlik, selectBlik},
			author: "A", command: showOwn,
			want: "<@A> :money_with_wings: \n- `blik` - 555 555 555 (default)\n- `bank` - PL61 1090",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			l := newLedger(t)
			for _, cmd := range tc.setup {
				applyCommand(t, l, "A", cmd)
			}

			// Act
			reply := applyCommand(t, l, tc.author, tc.command)

			// Assert
			if reply.Declined || reply.Text != tc.want {
				t.Fatalf("reply = %+v, want %q", reply, tc.want)
			}
		})
	}

	t.Run("declines a missing default", func(t *testing.T) {
		// Arrange
		l := newLedger(t)
		applyCommand(t, l, "A", saveBlik)
		applyCommand(t, l, "A", selectBlik)

		// Act
		reply := applyCommand(t, l, "A", parser.Command{Kind: parser.KindPaySetDefault, PayMethod: "wise"})

		// Assert
		want := "You haven't saved `wise` yet — add it first with `pay set wise ...`."
		if !reply.Declined || reply.Text != want {
			t.Fatalf("reply = %+v, want decline with %q", reply, want)
		}
	})

	t.Run("clearing methods preserves another user's details", func(t *testing.T) {
		// Arrange
		l := newLedger(t)
		applyCommand(t, l, "A", saveBank)
		applyCommand(t, l, "B", parser.Command{Kind: parser.KindPaySet, PayMethod: "revolut", PayValue: "@kamil"})

		// Act
		applyCommand(t, l, "A", clearMethods)

		// Assert
		reply := applyCommand(t, l, "A", parser.Command{Kind: parser.KindPayShowFor, Target: "B"})
		assertReplyText(t, reply, "<@B> :money_with_wings: \n- `revolut` - @kamil")
	})
}

func TestOverflowPreservesBalance(t *testing.T) {
	for _, tc := range []struct {
		name             string
		sign             int
		creditor, debtor string
	}{
		{name: "when the target owes", sign: 1, creditor: "A", debtor: "B"},
		{name: "when the author owes", sign: -1, creditor: "B", debtor: "A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			l := newLedger(t)
			add(t, l, "A", "B", tc.sign, math.MaxInt64, "EUR")
			cmd := parser.Command{
				Kind: parser.KindAdd, Target: "B", Sign: tc.sign, Minor: 2, Currency: "EUR",
			}

			// Act
			_, err := l.Apply(t.Context(), "A", cmd)

			// Assert
			if err == nil {
				t.Fatal("accepted an overflowing adjustment")
			}
			rows, readErr := l.store.ListPair(t.Context(), tc.creditor, tc.debtor)
			if readErr != nil || len(rows) != 1 || rows[0].AmountMinor != math.MaxInt64 {
				t.Fatalf("balance after overflow: %v, %v", rows, readErr)
			}
		})
	}
}

func TestCurrencyResetFromDebtor(t *testing.T) {
	// Arrange
	l := newLedger(t)
	add(t, l, "A", "B", 1, 500, "EUR")
	add(t, l, "A", "B", 1, 200, "PLN")
	cmd := parser.Command{Kind: parser.KindReset, Target: "A", Currency: "EUR"}

	// Act
	_, err := l.Apply(t.Context(), "B", cmd)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	rows, err := l.store.ListPair(t.Context(), "A", "B")
	if err != nil || len(rows) != 1 || rows[0].Currency != "PLN" || rows[0].AmountMinor != 200 {
		t.Fatalf("remaining balances: %v, %v", rows, err)
	}
}

func TestConcurrentAdjustmentsPreserveEveryCent(t *testing.T) {
	// Arrange
	l := newLedger(t)
	const writers = 24
	cmd := parser.Command{Kind: parser.KindAdd, Target: "B", Sign: 1, Minor: 1}
	start := make(chan struct{})
	results := make(chan error, writers)
	var group sync.WaitGroup
	for range writers {
		group.Go(func() {
			<-start
			_, err := l.Apply(t.Context(), "A", cmd)
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
			t.Errorf("adjustment: %v", err)
		}
	}
	rows, err := l.store.ListPair(t.Context(), "A", "B")
	if err != nil || len(rows) != 1 || rows[0].AmountMinor != writers {
		t.Fatalf("final balance: %v, %v; want %d cents", rows, err, writers)
	}
}

func TestRedeliveredOverflowStaysDeclinedAfterSettlement(t *testing.T) {
	// Arrange
	l := newLedger(t)
	add(t, l, "A", "B", 1, math.MaxInt64, "EUR")
	cmd := parser.Command{Kind: parser.KindAdd, Target: "B", Minor: 1, Sign: 1, Currency: "EUR"}
	_, firstErr := l.ApplyOnce(t.Context(), "charge", "A", cmd)
	if !errors.Is(firstErr, engine.ErrBalanceOverflow) {
		t.Fatalf("first delivery: %v", firstErr)
	}
	add(t, l, "A", "B", -1, math.MaxInt64, "EUR")

	// Act
	_, err := l.ApplyOnce(t.Context(), "charge", "A", cmd)

	// Assert
	if !errors.Is(err, engine.ErrBalanceOverflow) {
		t.Fatalf("redelivery: %v", err)
	}
	rows, readErr := l.store.ListPair(t.Context(), "A", "B")
	if readErr != nil || len(rows) != 0 {
		t.Fatalf("settled balance: %v, %v", rows, readErr)
	}
}

func TestOldPaymentUpdateDoesNotOverwriteNewDetailsAfterReopen(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), "restart.db")
	openLedger := func() *Ledger {
		t.Helper()
		s, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return New(s, "PLN", i18n.New("en"))
	}
	first := openLedger()
	original := parser.Command{Kind: parser.KindPaySet, PayMethod: "bank", PayValue: "old account"}
	if _, err := first.ApplyOnce(t.Context(), "first", "A", original); err != nil {
		t.Fatal(err)
	}
	newer := original
	newer.PayValue = "new account"
	if _, err := first.ApplyOnce(t.Context(), "second", "A", newer); err != nil {
		t.Fatal(err)
	}
	if err := first.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openLedger()
	defer func() { _ = reopened.store.Close() }()

	// Act
	_, err := reopened.ApplyOnce(t.Context(), "first", "A", original)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	methods, err := reopened.store.ListPaymentMethods(t.Context(), "A")
	if err != nil || len(methods) != 1 || methods[0].Value != newer.PayValue {
		t.Fatalf("payment details: %v, %v", methods, err)
	}
}

// Open a second connection to check what was actually committed by the command.
func ledgerWithJournal(t *testing.T) (*Ledger, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(st, "PLN", i18n.New("en")), db
}

type journalEntry struct {
	kind, actor, command, creditor, debtor, currency string
	amount                                           int64
}

func readJournal(t *testing.T, db *sql.DB) []journalEntry {
	t.Helper()
	rows, err := db.Query(`
		SELECT kind, actor_id, command_id, creditor_id, debtor_id, currency, amount_minor
		FROM ledger_entries ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var entries []journalEntry
	for rows.Next() {
		var e journalEntry
		err := rows.Scan(
			&e.kind, &e.actor, &e.command,
			&e.creditor, &e.debtor, &e.currency, &e.amount,
		)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestRedeliveredResetPreservesJournal(t *testing.T) {
	// Arrange
	l, db := ledgerWithJournal(t)
	apply := func(id, author string, cmd parser.Command) {
		t.Helper()
		if _, err := l.ApplyOnce(t.Context(), id, author, cmd); err != nil {
			t.Fatal(err)
		}
	}
	apply("lunch", "A", parser.Command{
		Kind: parser.KindAdd, Target: "B", Sign: 1, Minor: 2000, Currency: "EUR",
	})
	apply("refund", "A", parser.Command{
		Kind: parser.KindAdd, Target: "B", Sign: -1, Minor: 3000, Currency: "EUR",
	})
	apply("train", "A", parser.Command{
		Kind: parser.KindAdd, Target: "B", Sign: 1, Minor: 500, Currency: "PLN",
	})
	reset := parser.Command{Kind: parser.KindReset, Target: "A"}
	apply("settled", "B", reset)
	apply("coffee", "A", parser.Command{
		Kind: parser.KindAdd, Target: "B", Sign: 1, Minor: 300, Currency: "PLN",
	})

	// Act
	apply("settled", "B", reset)

	// Assert
	want := []journalEntry{
		{"adjustment", "A", "lunch", "A", "B", "EUR", 2000},
		{"adjustment", "A", "refund", "B", "A", "EUR", 3000},
		{"adjustment", "A", "train", "A", "B", "PLN", 500},
		{"reset", "B", "settled", "A", "B", "EUR", 1000},
		{"reset", "B", "settled", "B", "A", "PLN", 500},
		{"adjustment", "A", "coffee", "A", "B", "PLN", 300},
	}
	if got := readJournal(t, db); !slices.Equal(got, want) {
		t.Fatalf("journal after redelivery = %+v, want %+v", got, want)
	}
}

func TestAccountPostingsReconstructCurrentBalances(t *testing.T) {
	// Arrange
	l, db := ledgerWithJournal(t)
	add(t, l, "A", "B", 1, math.MaxInt64, "EUR")
	add(t, l, "A", "B", -1, math.MaxInt64, "EUR")
	add(t, l, "B", "A", -1, math.MaxInt64, "EUR")
	add(t, l, "B", "A", 1, 100, "PLN")
	add(t, l, "C", "B", 1, 700, "GBP")

	// Act
	var differences int
	row := db.QueryRow(`
		WITH reconstructed AS (
			SELECT account_id, counterparty_id, currency,
			       SUM(amount_minor ORDER BY entry_id) AS balance
			FROM account_postings
			GROUP BY account_id, counterparty_id, currency
			HAVING balance > 0
		), current AS (
			SELECT creditor_id, debtor_id, currency, amount_minor FROM debts
		)
		SELECT (SELECT count(*) FROM (SELECT * FROM reconstructed EXCEPT SELECT * FROM current))
		     + (SELECT count(*) FROM (SELECT * FROM current EXCEPT SELECT * FROM reconstructed))`)
	err := row.Scan(&differences)

	// Assert
	if err != nil || differences != 0 {
		t.Fatalf("reconstructed and stored balances differ by %d rows: %v", differences, err)
	}
}
