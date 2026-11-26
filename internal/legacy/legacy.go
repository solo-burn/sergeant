package legacy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"sergeant/internal/database"
	"sergeant/internal/engine"
	"sergeant/internal/i18n"
	"sergeant/internal/parser"
)

// Engine owns the authoritative database and the ordered feed for the ghost.
type Engine struct {
	db              *sql.DB
	defaultCurrency string
	t               *i18n.Translator
	writer          chan struct{}
}

func Open(path, currency string, tr *i18n.Translator) (*Engine, error) {
	db, err := database.OpenBalances(path)
	if err != nil {
		return nil, err
	}
	if err := initializeFeed(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Engine{db: db, defaultCurrency: currency, t: tr, writer: make(chan struct{}, 1)}, nil
}

func (l *Engine) Close() error { return l.db.Close() }

// ApplyOnce commits the live command, its receipt and its queue entry together.
// A storage error rolls all three back. Only this result goes back to Slack.
func (l *Engine) ApplyOnce(ctx context.Context, id, author string, cmd parser.Command) (engine.Reply, error) {
	select {
	case l.writer <- struct{}{}:
		defer func() { <-l.writer }()
	case <-ctx.Done():
		return engine.Reply{}, ctx.Err()
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return engine.Reply{}, err
	}
	defer func() { _ = tx.Rollback() }()
	needsReceipt := id != "" && cmd.Mutates()
	if needsReceipt {
		text, err := readReceipt(ctx, tx, id)
		if !errors.Is(err, sql.ErrNoRows) {
			return l.reply(cmd, text, err)
		}
	}
	if cmd.Kind == parser.KindAdd && cmd.Currency == "" {
		cmd.Currency = l.defaultCurrency
	}
	text, resultErr := l.apply(ctx, &transaction{conn: tx}, author, cmd)
	outcome, terminal := engine.Outcome(resultErr)
	if !terminal {
		return engine.Reply{}, resultErr
	}
	if needsReceipt {
		if err := saveReceipt(ctx, tx, id, text, outcome); err != nil {
			return engine.Reply{}, err
		}
	}
	delivery := engine.Delivery{
		ID:      id,
		Author:  author,
		Locale:  l.t.Locale(),
		Command: cmd,
	}
	if err := enqueue(ctx, tx, delivery); err != nil {
		return engine.Reply{}, err
	}
	if err := tx.Commit(); err != nil {
		return engine.Reply{}, err
	}
	return l.reply(cmd, text, resultErr)
}

func (l *Engine) reply(cmd parser.Command, text string, err error) (engine.Reply, error) {
	if errors.Is(err, engine.ErrUnknownMethod) {
		return engine.Reply{Text: l.t.T(i18n.PayDefaultMissing, cmd.PayMethod), Declined: true}, nil
	}
	return engine.Reply{Text: text}, err
}

func (l *Engine) apply(
	ctx context.Context, tx *transaction, author string, cmd parser.Command,
) (string, error) {
	switch cmd.Kind {
	case parser.KindAdd:
		return l.adjustDebt(ctx, tx, author, cmd)
	case parser.KindReset:
		return l.resetDebt(ctx, tx, author, cmd)
	case parser.KindStatusFor:
		rows, err := tx.ListPair(ctx, author, cmd.Target)
		if err != nil {
			return "", err
		}
		if len(rows) == 0 {
			return l.t.T(i18n.LedgerStatusForEmpty, cmd.Target), nil
		}
		return l.t.T(i18n.LedgerStatusFor, cmd.Target, joinAmounts(rows, nil)), nil

	case parser.KindStatusAll:
		return l.renderStatus(ctx, tx, author)

	case parser.KindSummary:
		return l.renderSummary(ctx, tx, author)

	case parser.KindPaySet:
		if err := tx.SetPaymentMethod(ctx, author, cmd.PayMethod, cmd.PayValue); err != nil {
			return "", err
		}
		return l.t.T(i18n.PaySaved, cmd.PayMethod), nil

	case parser.KindPaySetDefault:
		if err := tx.SetDefaultPaymentMethod(ctx, author, cmd.PayMethod); err != nil {
			return "", err
		}
		return l.t.T(i18n.PayDefaultSet, cmd.PayMethod), nil

	case parser.KindPayRemove:
		if err := tx.RemovePaymentMethod(ctx, author, cmd.PayMethod); err != nil {
			return "", err
		}
		return l.t.T(i18n.PayRemoved, cmd.PayMethod), nil

	case parser.KindPayClear:
		if err := tx.ClearPaymentMethods(ctx, author); err != nil {
			return "", err
		}
		return l.t.T(i18n.PayCleared), nil

	case parser.KindPayShowSelf:
		return l.renderPay(ctx, tx, author, true)

	case parser.KindPayShowFor:
		return l.renderPay(ctx, tx, cmd.Target, false)

	default:
		return "", errors.New("unknown command kind")
	}
}

func (l *Engine) adjustDebt(
	ctx context.Context, tx *transaction, author string, cmd parser.Command,
) (string, error) {
	if cmd.Target == author {
		return "", engine.ErrSelfTarget
	}
	if cmd.Minor < 0 || (cmd.Sign != 1 && cmd.Sign != -1) {
		return "", errors.New("invalid adjustment")
	}
	delta := cmd.Minor * int64(cmd.Sign)
	net, err := tx.AddDeltaWithSimplifiedDebt(ctx, author, cmd.Target, cmd.Currency, delta)
	if err != nil {
		return "", err
	}
	if net == 0 {
		return l.t.T(i18n.LedgerTabCleared, cmd.Target, cmd.Currency), nil
	}
	if net > 0 {
		return l.t.T(i18n.LedgerNowOwes, cmd.Target, formatMinor(net), cmd.Currency), nil
	}
	return l.t.T(i18n.LedgerYouOwe, cmd.Target, formatMinor(-net), cmd.Currency), nil
}

func (l *Engine) resetDebt(
	ctx context.Context, tx *transaction, author string, cmd parser.Command,
) (string, error) {
	if cmd.Target == author {
		return "", engine.ErrSelfTarget
	}
	if cmd.Currency == "" {
		if err := tx.ResetPair(ctx, author, cmd.Target); err != nil {
			return "", err
		}
		return l.t.T(i18n.LedgerResetAll, cmd.Target), nil
	}
	if err := tx.ResetPairCurrency(ctx, author, cmd.Target, cmd.Currency); err != nil {
		return "", err
	}
	return l.t.T(i18n.LedgerResetCurrency, cmd.Target, cmd.Currency), nil
}

func (l *Engine) renderStatus(ctx context.Context, tx *transaction, userID string) (string, error) {
	owedToUser, err := tx.ListByCreditor(ctx, userID)
	if err != nil {
		return "", err
	}
	owedByUser, err := tx.ListByDebtor(ctx, userID)
	if err != nil {
		return "", err
	}
	if len(owedToUser) == 0 && len(owedByUser) == 0 {
		return l.t.T(i18n.LedgerStatusAllEmpty), nil
	}

	paymentByCreditor := map[string]*paymentMethod{}
	for _, r := range owedByUser {
		if _, seen := paymentByCreditor[r.Creditor]; seen {
			continue
		}
		pm, ok, err := tx.DefaultPaymentMethod(ctx, r.Creditor)
		if err != nil {
			return "", err
		}
		if ok {
			paymentByCreditor[r.Creditor] = &pm
		} else {
			paymentByCreditor[r.Creditor] = nil
		}
	}

	var b strings.Builder
	if len(owedToUser) > 0 {
		b.WriteString(l.t.T(i18n.LedgerStatusAllOwedHeader))
		writeGroupedLines(&b, l.t, owedToUser, func(d debt) string { return d.Debtor }, nil)
	}
	if len(owedByUser) > 0 {
		if len(owedToUser) > 0 {
			b.WriteString("\n")
		}
		b.WriteString(l.t.T(i18n.LedgerStatusAllOweHeader))
		writeGroupedLines(&b, l.t, owedByUser, func(d debt) string { return d.Creditor }, paymentByCreditor)
	}
	return b.String(), nil
}

func (l *Engine) renderSummary(ctx context.Context, tx *transaction, userID string) (string, error) {
	owed, err := tx.ListByCreditor(ctx, userID)
	if err != nil {
		return "", err
	}
	if len(owed) == 0 {
		return l.t.T(i18n.LedgerSummaryEmpty), nil
	}

	var b strings.Builder
	b.WriteString(l.t.T(i18n.LedgerSummaryOwedHeader))
	writeGroupedLines(&b, l.t, owed, func(d debt) string { return d.Debtor }, nil)

	pms, err := tx.ListPaymentMethods(ctx, userID)
	if err != nil {
		return "", err
	}
	if len(pms) > 0 {
		b.WriteString("\n\n")
		b.WriteString(l.t.T(i18n.LedgerSummaryPayHeader))
		l.writePaymentMethods(&b, pms)
	}

	return b.String(), nil
}

func (l *Engine) renderPay(ctx context.Context, tx *transaction, userID string, isSelf bool) (string, error) {
	pms, err := tx.ListPaymentMethods(ctx, userID)
	if err != nil {
		return "", err
	}
	if len(pms) == 0 {
		if isSelf {
			return l.t.T(i18n.PayShowSelfEmpty), nil
		}
		return l.t.T(i18n.PayShowForEmpty, userID), nil
	}
	var b strings.Builder

	b.WriteString("<@" + userID + "> :money_with_wings: ")

	l.writePaymentMethods(&b, pms)
	return b.String(), nil
}

func (l *Engine) writePaymentMethods(b *strings.Builder, pms []paymentMethod) {
	for _, pm := range pms {
		id := i18n.PayLine
		if pm.IsDefault {
			id = i18n.PayLineDefault
		}
		b.WriteString(l.t.T(id, pm.Method, pm.Value))
	}
}

func writeGroupedLines(
	b *strings.Builder, t *i18n.Translator, rows []debt,
	key func(debt) string, payByKey map[string]*paymentMethod,
) {
	// Store queries return all currencies for the same person consecutively.
	for len(rows) > 0 {
		person := key(rows[0])
		end := 1
		for end < len(rows) && key(rows[end]) == person {
			end++
		}
		amounts := joinAmounts(rows[:end], payByKey[person])
		b.WriteString(t.T(i18n.LedgerStatusAllLine, person, amounts))
		rows = rows[end:]
	}
}

func joinAmounts(rows []debt, pay *paymentMethod) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		amount := formatMinor(r.AmountMinor)
		if pay != nil {
			parts[i] = fmt.Sprintf("%s %s (`%s %s`)", amount, r.Currency, pay.Method, pay.Value)
		} else {
			parts[i] = fmt.Sprintf("%s %s", amount, r.Currency)
		}
	}
	return strings.Join(parts, ", ")
}

func formatMinor(n int64) string {
	return fmt.Sprintf("%d.%02d", n/100, n%100)
}
