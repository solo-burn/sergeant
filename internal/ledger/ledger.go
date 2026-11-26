package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"sergeant/internal/engine"
	"sergeant/internal/i18n"
	"sergeant/internal/ledger/store"
	"sergeant/internal/parser"
)

type Ledger struct {
	store           *store.SQLite
	defaultCurrency string
	t               *i18n.Translator
}

func New(s *store.SQLite, defaultCurrency string, tr *i18n.Translator) *Ledger {
	return &Ledger{store: s, defaultCurrency: defaultCurrency, t: tr}
}

func (l *Ledger) Apply(ctx context.Context, author string, cmd parser.Command) (engine.Reply, error) {
	return l.ApplyOnce(ctx, "", author, cmd)
}

func (l *Ledger) ApplyOnce(ctx context.Context, id, author string, cmd parser.Command) (engine.Reply, error) {
	execute := func(tx *store.Transaction) (string, error) {
		return l.apply(ctx, tx, author, cmd)
	}
	var text string
	var err error
	if cmd.Mutates() {
		text, err = l.store.WriteCommand(ctx, id, execute)
	} else {
		text, err = l.store.ReadSnapshot(ctx, execute)
	}
	if errors.Is(err, engine.ErrUnknownMethod) {
		return engine.Reply{Text: l.t.T(i18n.PayDefaultMissing, cmd.PayMethod), Declined: true}, nil
	}
	return engine.Reply{Text: text}, err
}

func (l *Ledger) apply(
	ctx context.Context,
	tx *store.Transaction,
	author string, cmd parser.Command,
) (string, error) {
	switch cmd.Kind {
	case parser.KindAdd:
		return l.adjustDebt(ctx, tx, author, cmd)
	case parser.KindReset:
		return l.resetDebt(ctx, tx, author, cmd)
	case parser.KindStatusFor:
		return l.renderPairStatus(ctx, tx, author, cmd.Target)
	case parser.KindStatusAll:
		return l.renderStatus(ctx, tx, author)
	case parser.KindSummary:
		return l.renderSummary(ctx, tx, author)
	case parser.KindPayShowSelf:
		return l.renderPay(ctx, tx, author, true)
	case parser.KindPayShowFor:
		return l.renderPay(ctx, tx, cmd.Target, false)

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

	default:
		return "", errors.New("unknown command kind")
	}
}

func (l *Ledger) adjustDebt(
	ctx context.Context,
	tx *store.Transaction,
	author string, cmd parser.Command,
) (string, error) {
	if cmd.Target == author {
		return "", engine.ErrSelfTarget
	}
	if cmd.Minor < 0 || (cmd.Sign != 1 && cmd.Sign != -1) {
		return "", errors.New("invalid adjustment")
	}
	currency := cmd.Currency
	if currency == "" {
		currency = l.defaultCurrency
	}
	delta := cmd.Minor * int64(cmd.Sign)
	net, err := tx.AddDeltaWithSimplifiedDebt(ctx, author, cmd.Target, currency, delta)
	if err != nil {
		return "", err
	}
	if net == 0 {
		return l.t.T(i18n.LedgerTabCleared, cmd.Target, currency), nil
	}
	if net > 0 {
		return l.t.T(i18n.LedgerNowOwes, cmd.Target, formatMinor(net), currency), nil
	}
	return l.t.T(i18n.LedgerYouOwe, cmd.Target, formatMinor(-net), currency), nil
}

func (l *Ledger) resetDebt(
	ctx context.Context,
	tx *store.Transaction,
	author string, cmd parser.Command,
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

func (l *Ledger) renderPairStatus(
	ctx context.Context,
	tx *store.Transaction,
	author, target string,
) (string, error) {
	rows, err := tx.ListPair(ctx, author, target)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return l.t.T(i18n.LedgerStatusForEmpty, target), nil
	}
	return l.t.T(i18n.LedgerStatusFor, target, joinAmounts(rows, nil)), nil
}

func (l *Ledger) renderStatus(ctx context.Context, tx *store.Transaction, userID string) (string, error) {
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

	paymentByCreditor, err := defaultPaymentMethods(ctx, tx, owedByUser)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	if len(owedToUser) > 0 {
		b.WriteString(l.t.T(i18n.LedgerStatusAllOwedHeader))
		writeGroupedLines(&b, l.t, owedToUser, func(d store.Debt) string { return d.Debtor }, nil)
	}
	if len(owedByUser) > 0 {
		if len(owedToUser) > 0 {
			b.WriteString("\n")
		}
		b.WriteString(l.t.T(i18n.LedgerStatusAllOweHeader))
		creditor := func(d store.Debt) string { return d.Creditor }
		writeGroupedLines(&b, l.t, owedByUser, creditor, paymentByCreditor)
	}
	return b.String(), nil
}

func defaultPaymentMethods(
	ctx context.Context,
	tx *store.Transaction,
	debts []store.Debt,
) (map[string]*store.PaymentMethod, error) {
	paymentByCreditor := map[string]*store.PaymentMethod{}
	for _, r := range debts {
		if _, seen := paymentByCreditor[r.Creditor]; seen {
			continue
		}
		pm, ok, err := tx.DefaultPaymentMethod(ctx, r.Creditor)
		if err != nil {
			return nil, err
		}
		if ok {
			paymentByCreditor[r.Creditor] = &pm
		} else {
			paymentByCreditor[r.Creditor] = nil
		}
	}
	return paymentByCreditor, nil
}

func (l *Ledger) renderSummary(ctx context.Context, tx *store.Transaction, userID string) (string, error) {
	owed, err := tx.ListByCreditor(ctx, userID)
	if err != nil {
		return "", err
	}
	if len(owed) == 0 {
		return l.t.T(i18n.LedgerSummaryEmpty), nil
	}

	var b strings.Builder
	b.WriteString(l.t.T(i18n.LedgerSummaryOwedHeader))
	writeGroupedLines(&b, l.t, owed, func(d store.Debt) string { return d.Debtor }, nil)

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

func (l *Ledger) renderPay(
	ctx context.Context,
	tx *store.Transaction,
	userID string, isSelf bool,
) (string, error) {
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

func (l *Ledger) writePaymentMethods(b *strings.Builder, pms []store.PaymentMethod) {
	for _, pm := range pms {
		id := i18n.PayLine
		if pm.IsDefault {
			id = i18n.PayLineDefault
		}
		b.WriteString(l.t.T(id, pm.Method, pm.Value))
	}
}

func writeGroupedLines(
	b *strings.Builder, t *i18n.Translator, rows []store.Debt,
	key func(store.Debt) string,
	payByKey map[string]*store.PaymentMethod,
) {
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

func joinAmounts(rows []store.Debt, pay *store.PaymentMethod) string {
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

func (l *Ledger) Replay(ctx context.Context, d engine.Delivery) error {
	replay := *l
	replay.t = i18n.New(d.Locale)
	return l.store.Replay(ctx, d, func(tx *store.Transaction) (string, error) {
		return replay.apply(ctx, tx, d.Author, d.Command)
	})
}
