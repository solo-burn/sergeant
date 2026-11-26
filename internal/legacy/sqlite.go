// Package legacy implements the original net-balance account engine.
package legacy

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
	"sergeant/internal/engine"
)

type debt struct {
	Creditor    string `alias:"debts.creditor_id"`
	Debtor      string `alias:"debts.debtor_id"`
	Currency    string `alias:"debts.currency"`
	AmountMinor int64  `alias:"debts.amount_minor"`
}

type paymentMethod struct {
	Method    string `alias:"payment_methods.method"`
	Value     string `alias:"payment_methods.value"`
	IsDefault bool   `alias:"payment_methods.is_default"`
}

type transaction struct{ conn *sql.Tx }

func readReceipt(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	r := table.AppliedCommands
	stmt := r.SELECT(r.Reply, r.Outcome).WHERE(r.ID.EQ(sqlite.String(id)))
	var reply, outcome string
	if err := database.ScanRow(ctx, tx, stmt, &reply, &outcome); err != nil {
		return "", err
	}
	return reply, engine.OutcomeError(outcome)
}

func saveReceipt(ctx context.Context, tx *sql.Tx, id, reply, outcome string) error {
	r := table.AppliedCommands
	stmt := r.INSERT(r.ID, r.Reply, r.Outcome).VALUES(id, reply, outcome)
	_, err := stmt.ExecContext(ctx, tx)
	return err
}

func eitherDirection(first, second string) sqlite.BoolExpression {
	d := table.Debts
	a, b := sqlite.String(first), sqlite.String(second)
	forward := d.CreditorID.EQ(a).AND(d.DebtorID.EQ(b))
	reverse := d.CreditorID.EQ(b).AND(d.DebtorID.EQ(a))
	return forward.OR(reverse)
}

func (s *transaction) ResetPair(ctx context.Context, creditor, debtor string) error {
	_, err := table.Debts.DELETE().WHERE(eitherDirection(creditor, debtor)).ExecContext(ctx, s.conn)
	return err
}

func (tx *transaction) AddDeltaWithSimplifiedDebt(
	ctx context.Context, creditor, debtor, currency string, delta int64,
) (int64, error) {
	d := table.Debts
	signedAmount := sqlite.CASE().WHEN(d.CreditorID.EQ(sqlite.String(creditor))).THEN(d.AmountMinor).
		ELSE(sqlite.Int(0).SUB(d.AmountMinor))
	total := sqlite.COALESCE(sqlite.SUMi(sqlite.IntExp(signedAmount)), sqlite.Int(0))
	pair := eitherDirection(creditor, debtor).AND(d.Currency.EQ(sqlite.String(currency)))
	stmt := d.SELECT(total).WHERE(pair)
	var balance int64
	err := database.ScanRow(ctx, tx.conn, stmt, &balance)
	if err != nil {
		return 0, err
	}
	// Even when the debt reverses, its absolute value must fit in a SQLite INTEGER.
	aboveMaximum := delta > 0 && balance > math.MaxInt64-delta
	belowMinimum := delta < 0 && balance < -math.MaxInt64-delta
	if aboveMaximum || belowMinimum {
		return 0, engine.ErrBalanceOverflow
	}
	net := balance + delta
	if err := tx.ResetPairCurrency(ctx, creditor, debtor, currency); err != nil {
		return 0, err
	}
	if net == 0 {
		return 0, nil
	}
	amount := net
	if net < 0 {
		creditor, debtor = debtor, creditor
		amount = -net
	}
	now := time.Now().Unix()
	insert := d.INSERT(d.CreditorID, d.DebtorID, d.Currency, d.AmountMinor, d.InsertedAt, d.UpdatedAt).
		VALUES(creditor, debtor, currency, amount, now, now)
	_, err = insert.ExecContext(ctx, tx.conn)
	return net, err
}

func (s *transaction) ResetPairCurrency(ctx context.Context, creditor, debtor, currency string) error {
	d := table.Debts
	stmt := d.DELETE().WHERE(eitherDirection(creditor, debtor).AND(d.Currency.EQ(sqlite.String(currency))))
	_, err := stmt.ExecContext(ctx, s.conn)
	return err
}

func (s *transaction) ListPair(ctx context.Context, creditor, debtor string) ([]debt, error) {
	d := table.Debts
	pair := d.CreditorID.EQ(sqlite.String(creditor)).AND(d.DebtorID.EQ(sqlite.String(debtor)))
	return s.listDebts(ctx, pair, d.Currency.ASC())
}

func (s *transaction) ListByCreditor(ctx context.Context, creditor string) ([]debt, error) {
	d := table.Debts
	return s.listDebts(ctx, d.CreditorID.EQ(sqlite.String(creditor)), d.DebtorID.ASC(), d.Currency.ASC())
}

func (s *transaction) ListByDebtor(ctx context.Context, debtor string) ([]debt, error) {
	d := table.Debts
	return s.listDebts(ctx, d.DebtorID.EQ(sqlite.String(debtor)), d.CreditorID.ASC(), d.Currency.ASC())
}

func (s *transaction) listDebts(
	ctx context.Context, where sqlite.BoolExpression, order ...sqlite.OrderByClause,
) ([]debt, error) {
	d := table.Debts
	stmt := d.SELECT(d.CreditorID, d.DebtorID, d.Currency, d.AmountMinor).
		WHERE(where.AND(d.AmountMinor.GT(sqlite.Int(0)))).
		ORDER_BY(order...)
	var debts []debt
	err := stmt.QueryContext(ctx, s.conn, &debts)
	return debts, err
}

func (s *transaction) SetPaymentMethod(ctx context.Context, userID, method, value string) error {
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

func (s *transaction) SetDefaultPaymentMethod(ctx context.Context, userID, method string) error {
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

func (s *transaction) RemovePaymentMethod(ctx context.Context, userID, method string) error {
	p := table.PaymentMethods
	stmt := p.DELETE().WHERE(p.UserID.EQ(sqlite.String(userID)).AND(p.Method.EQ(sqlite.String(method))))
	_, err := stmt.ExecContext(ctx, s.conn)
	return err
}

func (s *transaction) ClearPaymentMethods(ctx context.Context, userID string) error {
	p := table.PaymentMethods
	_, err := p.DELETE().WHERE(p.UserID.EQ(sqlite.String(userID))).ExecContext(ctx, s.conn)
	return err
}

func selectPaymentMethods() sqlite.SelectStatement {
	p := table.PaymentMethods
	return p.SELECT(p.Method, p.Value, p.IsDefault)
}

func (s *transaction) DefaultPaymentMethod(ctx context.Context, userID string) (paymentMethod, bool, error) {
	p := table.PaymentMethods
	user := p.UserID.EQ(sqlite.String(userID))
	stmt := selectPaymentMethods().WHERE(user.AND(p.IsDefault.EQ(sqlite.Int(1))))
	var method paymentMethod
	err := stmt.QueryContext(ctx, s.conn, &method)
	if errors.Is(err, qrm.ErrNoRows) {
		return paymentMethod{}, false, nil
	}
	return method, err == nil, err
}

func (s *transaction) ListPaymentMethods(ctx context.Context, userID string) ([]paymentMethod, error) {
	p := table.PaymentMethods
	stmt := selectPaymentMethods().WHERE(p.UserID.EQ(sqlite.String(userID))).
		ORDER_BY(p.IsDefault.DESC(), p.Method.ASC())
	var methods []paymentMethod
	err := stmt.QueryContext(ctx, s.conn, &methods)
	return methods, err
}
