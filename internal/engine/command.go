// Package engine defines the command boundary between Slack and the account engines.
package engine

import (
	"context"
	"errors"
	"fmt"

	"sergeant/internal/parser"
)

var (
	ErrSelfTarget      = errors.New("creditor and debtor cannot be the same")
	ErrUnknownMethod   = errors.New("unknown payment method")
	ErrBalanceOverflow = errors.New("balance exceeds the supported amount")
)

// Outcome names are persisted in command receipts.
func Outcome(err error) (string, bool) {
	switch {
	case err == nil:
		return "applied", true
	case errors.Is(err, ErrSelfTarget):
		return "self_target", true
	case errors.Is(err, ErrUnknownMethod):
		return "missing_method", true
	case errors.Is(err, ErrBalanceOverflow):
		return "overflow", true
	default:
		return "", false
	}
}

func OutcomeError(outcome string) error {
	switch outcome {
	case "applied":
		return nil
	case "self_target":
		return ErrSelfTarget
	case "missing_method":
		return ErrUnknownMethod
	case "overflow":
		return ErrBalanceOverflow
	default:
		return fmt.Errorf("unknown command outcome %q", outcome)
	}
}

type Reply struct {
	Text     string
	Declined bool
}

type Engine interface {
	ApplyOnce(context.Context, string, string, parser.Command) (Reply, error)
}

// Delivery is one committed legacy command in the order it was executed.
// Currency and locale are resolved at intake so a later config change cannot alter replay.
type Delivery struct {
	Sequence           int64
	ID, Author, Locale string
	Command            parser.Command
}
