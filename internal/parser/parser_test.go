package parser

import (
	"errors"
	"slices"
	"testing"
)

func TestParseMessage(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []Command
	}{
		{
			name: "one adjustment per line",
			text: "<@U1> +39\n<@U2> +41\n<@U3> +43",
			want: []Command{
				{Kind: KindAdd, Target: "U1", Sign: 1, Minor: 3900},
				{Kind: KindAdd, Target: "U2", Sign: 1, Minor: 4100},
				{Kind: KindAdd, Target: "U3", Sign: 1, Minor: 4300},
			},
		},
		{
			name: "blank lines and CRLF around signed amounts and currencies",
			text: "\r\n <@U1|user> -2,50 EUR\r\n \r\n <@U2> 3 GBP \r\n",
			want: []Command{
				{Kind: KindAdd, Target: "U1", Sign: -1, Minor: 250, Currency: "EUR"},
				{Kind: KindAdd, Target: "U2", Sign: 1, Minor: 300, Currency: "GBP"},
			},
		},
		{
			name: "single query remains supported",
			text: "status",
			want: []Command{{Kind: KindStatusAll}},
		},
		{
			name: "multiline payment value remains a single command",
			text: "pay set bank PL61\n1090 0000",
			want: []Command{{Kind: KindPaySet, PayMethod: "bank", PayValue: "PL61 1090 0000"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			text := tc.text

			// Act
			got, err := ParseMessage(text)

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseMessageRejectsInvalidBatch(t *testing.T) {
	tests := []struct{ name, text string }{
		{name: "empty message", text: "\n \n"},
		{name: "invalid later amount", text: "<@U1> +39\n<@U2> +oops"},
		{name: "invalid later currency", text: "<@U1> +39\n<@U2> +41 PLNS"},
		{name: "query mixed with adjustment", text: "<@U1> +39\nstatus"},
		{name: "reset mixed with adjustment", text: "<@U1> +39\n<@U2> reset"},
		{name: "multiple entries on one line", text: "<@U1> +39 <@U2> +41"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			text := tc.text

			// Act
			got, err := ParseMessage(text)

			// Assert
			if !errors.Is(err, ErrUnrecognized) || len(got) != 0 {
				t.Fatalf("got %+v, %v; want no commands and ErrUnrecognized", got, err)
			}
		})
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want Command
		bad  bool
	}{
		{in: "<@U1> +20 PLN", want: Command{Kind: KindAdd, Target: "U1", Sign: 1, Minor: 2000, Currency: "PLN"}},
		{in: "<@U1> 20 PLN", want: Command{Kind: KindAdd, Target: "U1", Sign: 1, Minor: 2000, Currency: "PLN"}},
		{in: "<@U1> 2137", want: Command{Kind: KindAdd, Target: "U1", Sign: 1, Minor: 213700}},
		{in: "<@U1> +20", want: Command{Kind: KindAdd, Target: "U1", Sign: 1, Minor: 2000}},
		{in: "<@U1> +20.50 EUR", want: Command{Kind: KindAdd, Target: "U1", Sign: 1, Minor: 2050, Currency: "EUR"}},
		{in: "<@U1> +20,50 EUR", want: Command{Kind: KindAdd, Target: "U1", Sign: 1, Minor: 2050, Currency: "EUR"}},
		{in: "<@U1> 12,95 GBP", want: Command{Kind: KindAdd, Target: "U1", Sign: 1, Minor: 1295, Currency: "GBP"}},
		{in: "<@U1> -3,5 PLN", want: Command{Kind: KindAdd, Target: "U1", Sign: -1, Minor: 350, Currency: "PLN"}},
		{in: "<@U1> +0.01", want: Command{Kind: KindAdd, Target: "U1", Sign: 1, Minor: 1}},
		{in: "<@U1> +0,01", want: Command{Kind: KindAdd, Target: "U1", Sign: 1, Minor: 1}},
		{in: "<@U1> -5 PLN", want: Command{Kind: KindAdd, Target: "U1", Sign: -1, Minor: 500, Currency: "PLN"}},
		{in: "<@U1|name> +1", want: Command{Kind: KindAdd, Target: "U1", Sign: 1, Minor: 100}},
		{in: "<@U1> reset", want: Command{Kind: KindReset, Target: "U1"}},
		{in: "<@U1> reset pln", want: Command{Kind: KindReset, Target: "U1", Currency: "PLN"}},
		{in: "<@U1> status", want: Command{Kind: KindStatusFor, Target: "U1"}},
		{in: "<@U1> ?", want: Command{Kind: KindStatusFor, Target: "U1"}},
		{in: "status", want: Command{Kind: KindStatusAll}},
		{in: "?", want: Command{Kind: KindStatusAll}},
		{in: "  ?  ", want: Command{Kind: KindStatusAll}},
		{in: "  STATUS  ", want: Command{Kind: KindStatusAll}},
		{in: "  <@U1>   +20   PLN  ", want: Command{Kind: KindAdd, Target: "U1", Sign: 1, Minor: 2000, Currency: "PLN"}},

		// Payment-method forms.
		{in: "pay me", want: Command{Kind: KindPayShowSelf}},
		{in: "  PAY  ME  ", want: Command{Kind: KindPayShowSelf}},
		{in: "pay set bank PL61 1090 0000 1234 5678", want: Command{Kind: KindPaySet, PayMethod: "bank", PayValue: "PL61 1090 0000 1234 5678"}},
		{in: "pay set BLIK 555 555 555", want: Command{Kind: KindPaySet, PayMethod: "blik", PayValue: "555 555 555"}},
		{in: "pay set-default bank", want: Command{Kind: KindPaySetDefault, PayMethod: "bank"}},
		{in: "pay set-default BLIK", want: Command{Kind: KindPaySetDefault, PayMethod: "blik"}},
		{in: "pay rm bank", want: Command{Kind: KindPayRemove, PayMethod: "bank"}},
		{in: "pay remove bank", want: Command{Kind: KindPayRemove, PayMethod: "bank"}},
		{in: "pay clear", want: Command{Kind: KindPayClear}},
		{in: "pay set", want: Command{Kind: KindPaySetForm}},
		{in: "<@U1> pay", want: Command{Kind: KindPayShowFor, Target: "U1"}},

		{in: "help", want: Command{Kind: KindHelp}},
		{in: "  HELP  ", want: Command{Kind: KindHelp}},

		{in: "summary", want: Command{Kind: KindSummary}},
		{in: "  SUMMARY  ", want: Command{Kind: KindSummary}},

		{in: "hello", want: Command{Kind: KindHello}},
		{in: "  HELLO  ", want: Command{Kind: KindHello}},

		{in: "", bad: true},
		{in: "<@U1> +abc", bad: true},      // bad amount
		{in: "<@U1> +20 PLNS", bad: true},  // bad currency
		{in: "<@U1> +20.123", bad: true},   // too many fraction digits
		{in: "<@U1> +20,123", bad: true},   // too many fraction digits (comma)
		{in: "<@U1> +20,", bad: true},      // trailing comma
		{in: "<@U1> +20.", bad: true},      // trailing dot
		{in: "<@U1> +1,2,3", bad: true},    // multiple separators
		{in: "<@U1> pay 20", bad: true},    // pay takes no args after target
		{in: "pay set bank", bad: true},    // missing value
		{in: "pay set-default", bad: true}, // missing method
		{in: "pay set-default a b", bad: true},
		{in: "pay set-default BAD!", bad: true},
		{in: "pay set BAD! value", bad: true}, // invalid method chars
		{in: "pay rm", bad: true},
		{in: "pay clear extra", bad: true},
		{in: "pay nonsense", bad: true},
		{in: "pay", bad: true}, // bare pay no longer recognized
		{in: "pay me extra", bad: true},
		{in: "<@U1> status now", bad: true},
		{in: "<@U1> reset PLN extra", bad: true},
		{in: "<@U1> +99999999999999999999 PLN", bad: true}, // exceeds int64
		{in: "<@U1> +100000000000000000 PLN", bad: true},   // 1e17, would overflow on *100
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := Parse(tc.in)
			if tc.bad {
				if !errors.Is(err, ErrUnrecognized) {
					t.Fatalf("want ErrUnrecognized, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
