package shadow

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"strconv"
	"testing"

	"sergeant/internal/engine"
	"sergeant/internal/parser"
)

// A key is oriented alphabetically. A positive balance means the second account
// owes the first. The oracle uses exact integers before checking the storage limit.
type tab struct{ first, second, currency string }
type balances map[tab]int64

const selectDebts = "SELECT creditor_id, debtor_id, currency, amount_minor FROM debts"

type debtRequest struct {
	id, author string
	cmd        parser.Command
}

func (b balances) apply(r debtRequest) (balances, error) {
	cmd := r.cmd
	if r.author == cmd.Target {
		return nil, engine.ErrSelfTarget
	}
	first, second := min(r.author, cmd.Target), max(r.author, cmd.Target)
	if cmd.Kind == parser.KindReset {
		postings := balances{}
		for k, amount := range b {
			if k.first == first && k.second == second && (cmd.Currency == "" || cmd.Currency == k.currency) {
				delete(b, k)
				postings[k] = -amount
			}
		}
		return postings, nil
	}
	currency := cmd.Currency
	if currency == "" {
		currency = "PLN"
	}
	k := tab{first, second, currency}
	delta := int64(cmd.Sign) * cmd.Minor
	if r.author != first {
		delta = -delta
	}
	if delta == 0 {
		return nil, nil
	}
	next := new(big.Int).Add(big.NewInt(b[k]), big.NewInt(delta))
	if next.BitLen() > 63 {
		return nil, engine.ErrBalanceOverflow
	}
	if next.Sign() == 0 {
		delete(b, k)
	} else {
		b[k] = next.Int64()
	}
	return balances{k: delta}, nil
}

const (
	chargeTab byte = iota
	refundTab
	resetCurrency
	resetPair
	retryRequest
	numActions
)

// Each triple selects an action, a pair and currency, and an amount (or retry index).
// Keeping choices local lets the Go fuzzer shrink and recombine command histories.
func decodeHistory(program []byte, amount int64) []debtRequest {
	pairs := [...][2]string{{"A", "B"}, {"B", "A"}, {"C", "B"}, {"A", "C"}, {"A", "A"}}
	currencies := [...]string{"PLN", "EUR", ""}
	amounts := [...]int64{0, 1, 2, 100, math.MaxInt64 - 1, math.MaxInt64, amount & math.MaxInt64}
	var history []debtRequest
	for len(program) >= 3 && len(history) < 32 {
		action, route, value := program[0]%numActions, int(program[1]), int(program[2])
		program = program[3:]
		if action == retryRequest && len(history) > 0 {
			history = append(history, history[value%len(history)])
			continue
		}
		pair := pairs[route%len(pairs)]
		currency := currencies[route/len(pairs)%len(currencies)]
		cmd := parser.Command{Kind: parser.KindAdd, Sign: 1, Minor: amounts[value%len(amounts)], Target: pair[1], Currency: currency}
		switch action {
		case refundTab:
			cmd.Sign = -1
		case resetCurrency, resetPair:
			cmd = parser.Command{Kind: parser.KindReset, Target: pair[1], Currency: currency}
			if action == resetPair {
				cmd.Currency = ""
			}
		}
		history = append(history, debtRequest{strconv.Itoa(len(history)), pair[0], cmd})
	}
	return history
}

func assertAmounts(t *testing.T, source string, want balances, db *sql.DB, query string, args ...any) {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := balances{}
	for rows.Next() {
		var k tab
		var n int64
		if err := rows.Scan(&k.first, &k.second, &k.currency, &n); err != nil {
			t.Fatal(err)
		}
		if k.first == k.second || n <= 0 {
			t.Fatalf("%s has invalid debt %+v = %d", source, k, n)
		}
		if k.first > k.second {
			k.first, k.second = k.second, k.first
			n = -n
		}
		if _, found := got[k]; found {
			t.Fatalf("%s has debts in both directions for %+v", source, k)
		}
		got[k] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("%s balances %v, want %v", source, got, want)
	}
}

func FuzzDebtHistory(f *testing.F) {
	// Every pair of signed boundary operands runs with ordinary go test.
	// Magnitude selectors are zero, one, two, maximum minus one and maximum.
	boundaries := []byte{0, 1, 2, 4, 5}
	for _, openingSign := range []byte{chargeTab, refundTab} {
		for _, openingAmount := range boundaries {
			var program []byte
			for _, sign := range []byte{chargeTab, refundTab} {
				for _, amount := range boundaries {
					program = append(program, resetPair, 0, 0, openingSign, 0, openingAmount, sign, 0, amount)
				}
			}
			f.Add(program, int64(0))
		}
	}
	// Overflow, settlement, retry of the decline, then a multi-currency reset
	// and its redelivery after a new default-currency charge.
	f.Add([]byte{
		chargeTab, 0, 5, chargeTab, 0, 1, refundTab, 0, 5, retryRequest, 0, 1,
		refundTab, 1, 4, chargeTab, 5, 2, chargeTab, 2, 6, resetPair, 1, 0,
		chargeTab, 10, 3, retryRequest, 0, 7,
	}, int64(71235))
	// Negative limit, zero adjustment, self-target rejection, and scoped resets.
	f.Add([]byte{
		refundTab, 0, 5, refundTab, 0, 1, chargeTab, 1, 0, chargeTab, 4, 1,
		chargeTab, 5, 1, resetCurrency, 6, 0, resetCurrency, 6, 0, retryRequest, 0, 3,
	}, int64(1))
	f.Fuzz(func(t *testing.T, program []byte, amount int64) {
		// Arrange
		history := decodeHistory(program, amount)
		if len(history) == 0 {
			t.Skip("no complete command")
		}
		trial := newTrial(t)
		ghost := trial.ledger(t)
		primaryDB, journalDB := connect(t, trial.primary), connect(t, trial.ghost)
		want := balances{}
		type result struct {
			reply    engine.Reply
			err      error
			postings balances
		}
		receipts := map[string]result{}
		wantEntries := 0

		for step, r := range history {
			// Arrange
			expected, retry := receipts[r.id]
			if !retry {
				expected.postings, expected.err = want.apply(r)
				wantEntries += len(expected.postings)
			}

			// Act
			reply, err := trial.Legacy.ApplyOnce(t.Context(), r.id, r.author, r.cmd)
			replayErr := trial.drain(t.Context(), ghost)

			// Assert
			if !errors.Is(err, expected.err) || replayErr != nil {
				t.Fatalf("step %d %+v: got %v, want %v, replay %v", step, r, err, expected.err, replayErr)
			}
			if !retry {
				expected.reply = reply
				receipts[r.id] = expected
			} else if reply != expected.reply {
				t.Fatalf("step %d redelivery: reply %+v, originally %+v", step, reply, expected.reply)
			}
			assertReceipt(t, primaryDB, r.id, reply.Text, expected.err)
			assertReceipt(t, journalDB, r.id, reply.Text, expected.err)
			assertAmounts(t, fmt.Sprintf("step %d legacy", step), want, primaryDB, selectDebts)
			assertAmounts(t, fmt.Sprintf("step %d journal", step), want, journalDB, selectDebts)
			kind := "adjustment"
			if r.cmd.Kind == parser.KindReset {
				kind = "reset"
			}
			assertAmounts(t, fmt.Sprintf("step %d postings", step), expected.postings, journalDB,
				`SELECT creditor_id, debtor_id, currency, amount_minor FROM ledger_entries
				 WHERE command_id=? AND actor_id=? AND kind=?`, r.id, r.author, kind)
			var entries, sequence int
			err = journalDB.QueryRow(`SELECT (SELECT count(*) FROM ledger_entries), sequence FROM shadow_trial`).
				Scan(&entries, &sequence)
			if err != nil || entries != wantEntries || sequence != len(receipts) {
				t.Fatalf("step %d: entries=%d want %d, sequence=%d want %d, err=%v",
					step, entries, wantEntries, sequence, len(receipts), err)
			}
		}
	})
}

func assertReceipt(t *testing.T, db *sql.DB, id, wantText string, rejection error) {
	t.Helper()
	wantOutcome := "applied"
	switch rejection {
	case engine.ErrSelfTarget:
		wantOutcome = "self_target"
	case engine.ErrBalanceOverflow:
		wantOutcome = "overflow"
	}
	var text, outcome string
	err := db.QueryRow("SELECT reply, outcome FROM applied_commands WHERE id=?", id).Scan(&text, &outcome)
	if err != nil || text != wantText || outcome != wantOutcome {
		t.Fatalf("receipt %s = (%q, %q), want (%q, %q): %v", id, text, outcome, wantText, wantOutcome, err)
	}
}
