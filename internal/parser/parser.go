package parser

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind values are persisted in the shadow command queue.
type Kind string

const (
	KindAdd           Kind = "add"
	KindReset         Kind = "reset"
	KindStatusFor     Kind = "status_for"
	KindStatusAll     Kind = "status_all"
	KindPaySet        Kind = "pay_set"
	KindPaySetForm    Kind = "pay_set_form"
	KindPaySetDefault Kind = "pay_set_default"
	KindPayRemove     Kind = "pay_remove"
	KindPayClear      Kind = "pay_clear"
	KindPayShowSelf   Kind = "pay_show_self"
	KindPayShowFor    Kind = "pay_show_for"
	KindHelp          Kind = "help"
	KindHello         Kind = "hello"
	KindSummary       Kind = "summary"
)

type Command struct {
	Kind      Kind
	Target    string
	Sign      int   // +1 or -1, set on KindAdd
	Minor     int64 // amount in 1/100 of major unit, set on KindAdd
	Currency  string
	PayMethod string // e.g. "bank", "blik", "revolut" - lowercase, [a-z0-9-]
	PayValue  string // free-form, trimmed
}

func (c Command) Mutates() bool {
	switch c.Kind {
	case KindAdd, KindReset, KindPaySet, KindPaySetDefault, KindPayRemove, KindPayClear:
		return true
	default:
		return false
	}
}

var ErrUnrecognized = errors.New("unrecognized command")

var (
	mentionRE = regexp.MustCompile(`^<@([A-Z0-9]+)(?:\|[^>]*)?>`)
	amountRE  = regexp.MustCompile(`^([+-]?)(\d+)(?:[.,](\d{1,2}))?$`)
)

// ParseMessage accepts a single command or one debt adjustment per non-empty line.
func ParseMessage(text string) ([]Command, error) {
	text = strings.TrimSpace(text)
	firstLine, _, multiline := strings.Cut(text, "\n")
	if !multiline || startsInlinePayment(firstLine) {
		cmd, err := Parse(text)
		if err != nil {
			return nil, err
		}
		return []Command{cmd}, nil
	}
	var commands []Command
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		cmd, err := Parse(line)
		if err != nil || cmd.Kind != KindAdd {
			return nil, ErrUnrecognized
		}
		commands = append(commands, cmd)
	}
	return commands, nil
}

// Inline payment details may wrap onto subsequent lines after the method.
func startsInlinePayment(line string) bool {
	fields := strings.Fields(line)
	return len(fields) >= 3 && strings.EqualFold(fields[0], "pay") && strings.EqualFold(fields[1], "set")
}

func Parse(text string) (Command, error) {
	text = strings.TrimSpace(text)
	if text == "?" || strings.EqualFold(text, "status") {
		return Command{Kind: KindStatusAll}, nil
	}
	if strings.EqualFold(text, "help") {
		return Command{Kind: KindHelp}, nil
	}
	if strings.EqualFold(text, "summary") {
		return Command{Kind: KindSummary}, nil
	}
	if strings.EqualFold(text, "hello") {
		return Command{Kind: KindHello}, nil
	}
	if strings.EqualFold(text, "payme") {
		return Command{Kind: KindPayShowSelf}, nil
	}

	if fields := strings.Fields(text); len(fields) > 0 && strings.EqualFold(fields[0], "pay") {
		return parsePay(fields)
	}

	m := mentionRE.FindStringSubmatch(text)
	if m == nil {
		return Command{}, ErrUnrecognized
	}
	target := m[1]
	fields := strings.Fields(text[len(m[0]):])
	if len(fields) == 0 {
		return Command{}, ErrUnrecognized
	}

	switch first := fields[0]; {
	case first == "?" || strings.EqualFold(first, "status"):
		if len(fields) != 1 {
			return Command{}, ErrUnrecognized
		}
		return Command{Kind: KindStatusFor, Target: target}, nil

	case strings.EqualFold(first, "pay"):
		if len(fields) != 1 {
			return Command{}, ErrUnrecognized
		}
		return Command{Kind: KindPayShowFor, Target: target}, nil

	case strings.EqualFold(first, "reset"):
		if len(fields) > 2 {
			return Command{}, ErrUnrecognized
		}
		c := Command{Kind: KindReset, Target: target}
		if len(fields) == 2 {
			ccy, err := NormalizeCurrency(fields[1])
			if err != nil {
				return Command{}, ErrUnrecognized
			}
			c.Currency = ccy
		}
		return c, nil

	default:
		if len(fields) > 2 {
			return Command{}, ErrUnrecognized
		}
		am := amountRE.FindStringSubmatch(fields[0])
		if am == nil {
			return Command{}, ErrUnrecognized
		}
		minor, err := parseMinor(am[2], am[3])
		if err != nil {
			return Command{}, ErrUnrecognized
		}
		sign := 1
		if am[1] == "-" {
			sign = -1
		}
		c := Command{Kind: KindAdd, Target: target, Sign: sign, Minor: minor}
		if len(fields) == 2 {
			ccy, err := NormalizeCurrency(fields[1])
			if err != nil {
				return Command{}, ErrUnrecognized
			}
			c.Currency = ccy
		}
		return c, nil
	}
}

func parsePay(fields []string) (Command, error) {
	if len(fields) == 1 {
		return Command{}, ErrUnrecognized
	}
	switch sub := strings.ToLower(fields[1]); sub {
	case "me":
		if len(fields) != 2 {
			return Command{}, ErrUnrecognized
		}
		return Command{Kind: KindPayShowSelf}, nil
	case "set":
		if len(fields) == 2 {
			return Command{Kind: KindPaySetForm}, nil
		}
		if len(fields) < 4 {
			return Command{}, ErrUnrecognized
		}
		method, ok := NormalizeMethod(fields[2])
		if !ok {
			return Command{}, ErrUnrecognized
		}
		value := strings.Join(fields[3:], " ")
		if !ValidPayValue(value) {
			return Command{}, ErrUnrecognized
		}
		return Command{Kind: KindPaySet, PayMethod: method, PayValue: value}, nil
	case "set-default":
		if len(fields) != 3 {
			return Command{}, ErrUnrecognized
		}
		method, ok := NormalizeMethod(fields[2])
		if !ok {
			return Command{}, ErrUnrecognized
		}
		return Command{Kind: KindPaySetDefault, PayMethod: method}, nil
	case "rm", "remove":
		if len(fields) != 3 {
			return Command{}, ErrUnrecognized
		}
		method, ok := NormalizeMethod(fields[2])
		if !ok {
			return Command{}, ErrUnrecognized
		}
		return Command{Kind: KindPayRemove, PayMethod: method}, nil
	case "clear":
		if len(fields) != 2 {
			return Command{}, ErrUnrecognized
		}
		return Command{Kind: KindPayClear}, nil
	}
	return Command{}, ErrUnrecognized
}

// ValidPayValue reports whether a trimmed payment value contains 1 to 200 Unicode code points in valid UTF-8.
func ValidPayValue(value string) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 200
}

// NormalizeMethod returns a lowercase method label or false if invalid.
// Allowed: 1-20 chars of [a-z0-9-].
func NormalizeMethod(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) == 0 || len(s) > 20 {
		return "", false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return "", false
		}
	}
	return s, true
}

func NormalizeCurrency(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 3 {
		return "", errors.New("invalid currency")
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return "", errors.New("invalid currency")
		}
	}
	return s, nil
}

func parseMinor(whole, fraction string) (int64, error) {
	switch len(fraction) {
	case 0:
		fraction = "00"
	case 1:
		fraction += "0"
	}
	return strconv.ParseInt(whole+fraction, 10, 64)
}
