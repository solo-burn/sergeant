// Package store persists the debt journal, current balances and payment methods in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/go-jet/jet/v2/qrm"
	"github.com/go-jet/jet/v2/sqlite"

	"sergeant/internal/database"
	"sergeant/internal/database/jet/table"
	"sergeant/internal/database/schema"
	"sergeant/internal/engine"
)

type Debt struct {
	Creditor    string `alias:"debts.creditor_id"`
	Debtor      string `alias:"debts.debtor_id"`
	Currency    string `alias:"debts.currency"`
	AmountMinor int64  `alias:"debts.amount_minor"`
	InsertedAt  int64  `alias:"debts.inserted_at"`
}

type PaymentMethod struct {
	UserID     string `alias:"payment_methods.user_id"`
	Method     string `alias:"payment_methods.method"`
	Value      string `alias:"payment_methods.value"`
	IsDefault  bool   `alias:"payment_methods.is_default"`
	InsertedAt int64  `alias:"payment_methods.inserted_at"`
}

type queries struct {
	conn      qrm.DB
	commandID string // Slack delivery receipt, when the write has one.
}

type SQLite struct {
	queries
	db     *sql.DB
	writer chan struct{}
}

type Transaction struct{ queries }

// ReadSnapshot lets a report read balances and payment details as of the same instant.
func (s *SQLite) ReadSnapshot(ctx context.Context, read func(*Transaction) (string, error)) (string, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	report, err := read(&Transaction{queries: queries{conn: tx}})
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return report, nil
}

// WriteCommand commits the mutation and its receipt together. An empty receipt ID
// applies the command without recording it for replay.
func (s *SQLite) WriteCommand(
	ctx context.Context,
	receiptID string,
	apply func(*Transaction) (string, error),
) (string, error) {
	// Queue commands from this store before taking a database connection.
	// Readers can still take snapshots while another goroutine is writing.
	select {
	case s.writer <- struct{}{}:
		defer func() { <-s.writer }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if receiptID != "" {
		reply, err := readReceipt(ctx, tx, receiptID)
		if !errors.Is(err, sql.ErrNoRows) {
			return reply, err
		}
	}
	reply, resultErr := apply(&Transaction{queries: queries{conn: tx, commandID: receiptID}})
	// These business rules reject the command before changing any rows.
	// A later balance or method change must not turn its retry into a write.
	outcome, terminal := engine.Outcome(resultErr)
	if !terminal {
		return "", resultErr
	}
	if receiptID != "" {
		_, err := insertReceipt(receiptID, reply, outcome).ExecContext(ctx, tx)
		if err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return reply, resultErr
}

func insertReceipt(id, reply, outcome string) sqlite.InsertStatement {
	r := table.AppliedCommands
	return r.INSERT(r.ID, r.Reply, r.Outcome).VALUES(id, reply, outcome)
}

func readReceipt(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var reply, outcome string
	r := table.AppliedCommands
	stmt := r.SELECT(r.Reply, r.Outcome).WHERE(r.ID.EQ(sqlite.String(id)))
	err := database.ScanRow(ctx, tx, stmt, &reply, &outcome)
	if err != nil {
		return "", err
	}
	return reply, engine.OutcomeError(outcome)
}

// Open opens (or creates) the database at path and applies migrations.
func Open(path string) (*SQLite, error) {
	db, err := database.OpenBalances(path, schema.Ledger)
	if err != nil {
		return nil, err
	}
	return &SQLite{db: db, queries: queries{conn: db}, writer: make(chan struct{}, 1)}, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

// balance is positive when the other person owes the named account.
func (tx *Transaction) balance(ctx context.Context, account, other, currency string) (int64, error) {
	d := table.Debts
	signedAmount := sqlite.CASE().WHEN(d.CreditorID.EQ(sqlite.String(account))).THEN(d.AmountMinor).
		ELSE(sqlite.Int(0).SUB(d.AmountMinor))
	total := sqlite.COALESCE(sqlite.SUMi(sqlite.IntExp(signedAmount)), sqlite.Int(0))
	pair := eitherDirection(account, other).AND(d.Currency.EQ(sqlite.String(currency)))
	stmt := d.SELECT(total).WHERE(pair)
	var balance int64
	err := database.ScanRow(ctx, tx.conn, stmt, &balance)
	return balance, err
}

// AddDeltaWithSimplifiedDebt posts an adjustment and returns the author's new net balance.
func (tx *Transaction) AddDeltaWithSimplifiedDebt(
	ctx context.Context,
	author, other, currency string, delta int64,
) (int64, error) {
	balance, err := tx.balance(ctx, author, other, currency)
	if err != nil {
		return 0, err
	}
	if delta == 0 {
		return balance, nil
	}
	// Both the entry amount and the resulting debt must fit in a positive int64.
	aboveMaximum := delta > 0 && balance > math.MaxInt64-delta
	belowMinimum := delta < 0 && balance < -math.MaxInt64-delta
	if delta == math.MinInt64 || aboveMaximum || belowMinimum {
		return 0, engine.ErrBalanceOverflow
	}
	creditor, debtor, amount := author, other, delta
	if delta < 0 {
		creditor, debtor, amount = other, author, -delta
	}
	e := table.LedgerEntries
	stmt := e.INSERT(e.Kind, e.ActorID, e.CommandID, e.CreditorID, e.DebtorID, e.Currency, e.AmountMinor).
		VALUES("adjustment", author, sqlite.NULLIF(sqlite.String(tx.commandID), sqlite.String("")),
			creditor, debtor, currency, amount)
	_, err = stmt.ExecContext(ctx, tx.conn)
	if err != nil {
		return 0, err
	}
	return balance + delta, nil
}

func eitherDirection(first, second string) sqlite.BoolExpression {
	d := table.Debts
	a, b := sqlite.String(first), sqlite.String(second)
	forward := d.CreditorID.EQ(a).AND(d.DebtorID.EQ(b))
	reverse := d.CreditorID.EQ(b).AND(d.DebtorID.EQ(a))
	return forward.OR(reverse)
}

func (s *queries) ResetPair(ctx context.Context, author, other string) error {
	return s.ResetPairCurrency(ctx, author, other, "")
}

// A reset posts the opposite of each outstanding balance. Materialize the
// balances first because posting an entry updates debts through a trigger.
func (s *queries) ResetPairCurrency(ctx context.Context, author, other, currency string) error {
	stmt := sqlite.RawStatement(`
		WITH outstanding AS MATERIALIZED (
			SELECT creditor_id, debtor_id, currency, amount_minor FROM debts
			WHERE ((creditor_id = :author AND debtor_id = :other)
			    OR (creditor_id = :other AND debtor_id = :author))
			  AND (:currency = '' OR currency = :currency)
		)
		INSERT INTO ledger_entries(
			kind, actor_id, command_id, creditor_id, debtor_id, currency, amount_minor
		)
		SELECT 'reset', :author, NULLIF(:command, ''), debtor_id, creditor_id, currency, amount_minor
		FROM outstanding ORDER BY currency`, sqlite.RawArgs{
		":author": author, ":other": other, ":currency": currency, ":command": s.commandID,
	})
	_, err := stmt.ExecContext(ctx, s.conn)
	return err
}

func (s *queries) ListPair(ctx context.Context, creditor, debtor string) ([]Debt, error) {
	d := table.Debts
	pair := d.CreditorID.EQ(sqlite.String(creditor)).AND(d.DebtorID.EQ(sqlite.String(debtor)))
	return s.listDebts(ctx, pair, d.Currency.ASC())
}

func (s *queries) ListByCreditor(ctx context.Context, creditor string) ([]Debt, error) {
	d := table.Debts
	return s.listDebts(ctx, d.CreditorID.EQ(sqlite.String(creditor)), d.DebtorID.ASC(), d.Currency.ASC())
}

func (s *queries) ListByDebtor(ctx context.Context, debtor string) ([]Debt, error) {
	d := table.Debts
	return s.listDebts(ctx, d.DebtorID.EQ(sqlite.String(debtor)), d.CreditorID.ASC(), d.Currency.ASC())
}

func (s *queries) listDebts(
	ctx context.Context, where sqlite.BoolExpression, order ...sqlite.OrderByClause,
) ([]Debt, error) {
	d := table.Debts
	stmt := d.SELECT(d.CreditorID, d.DebtorID, d.Currency, d.AmountMinor, d.InsertedAt).
		WHERE(where.AND(d.AmountMinor.GT(sqlite.Int(0)))).
		ORDER_BY(order...)
	var debts []Debt
	err := stmt.QueryContext(ctx, s.conn, &debts)
	return debts, err
}

func (s *queries) SetPaymentMethod(ctx context.Context, userID, method, value string) error {
	p := table.PaymentMethods
	now := time.Now().Unix()
	stmt := p.INSERT(p.UserID, p.Method, p.Value, p.InsertedAt, p.UpdatedAt).
		VALUES(userID, method, value, now, now).
		ON_CONFLICT(p.UserID, p.Method).DO_UPDATE(sqlite.SET(
		p.Value.SET(p.EXCLUDED.Value),
		p.UpdatedAt.SET(p.EXCLUDED.UpdatedAt),
	))
	_, err := stmt.ExecContext(ctx, s.conn)
	return err
}

func (s *queries) SetDefaultPaymentMethod(ctx context.Context, userID, method string) error {
	p := table.PaymentMethods
	user := p.UserID.EQ(sqlite.String(userID))
	selected := p.Method.EQ(sqlite.String(method))
	exists := sqlite.EXISTS(p.SELECT(p.Method).WHERE(user.AND(selected)))
	stmt := p.UPDATE(p.IsDefault, p.UpdatedAt).
		SET(selected, sqlite.Func("unixepoch")).
		WHERE(user.AND(exists))
	result, err := stmt.ExecContext(ctx, s.conn)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return engine.ErrUnknownMethod
	}
	return nil
}

func (s *queries) RemovePaymentMethod(ctx context.Context, userID, method string) error {
	p := table.PaymentMethods
	stmt := p.DELETE().WHERE(p.UserID.EQ(sqlite.String(userID)).AND(p.Method.EQ(sqlite.String(method))))
	_, err := stmt.ExecContext(ctx, s.conn)
	return err
}

func (s *queries) ClearPaymentMethods(ctx context.Context, userID string) error {
	p := table.PaymentMethods
	_, err := p.DELETE().WHERE(p.UserID.EQ(sqlite.String(userID))).ExecContext(ctx, s.conn)
	return err
}

func selectPaymentMethods() sqlite.SelectStatement {
	p := table.PaymentMethods
	return p.SELECT(p.UserID, p.Method, p.Value, p.IsDefault, p.InsertedAt)
}

func (s *queries) DefaultPaymentMethod(ctx context.Context, userID string) (PaymentMethod, bool, error) {
	p := table.PaymentMethods
	user := p.UserID.EQ(sqlite.String(userID))
	stmt := selectPaymentMethods().WHERE(user.AND(p.IsDefault.EQ(sqlite.Int(1))))
	var method PaymentMethod
	err := stmt.QueryContext(ctx, s.conn, &method)
	if errors.Is(err, qrm.ErrNoRows) {
		return PaymentMethod{}, false, nil
	}
	return method, err == nil, err
}

func (s *queries) ListPaymentMethods(ctx context.Context, userID string) ([]PaymentMethod, error) {
	p := table.PaymentMethods
	stmt := selectPaymentMethods().WHERE(p.UserID.EQ(sqlite.String(userID))).
		ORDER_BY(p.IsDefault.DESC(), p.Method.ASC())
	var methods []PaymentMethod
	err := stmt.QueryContext(ctx, s.conn, &methods)
	return methods, err
}
