# sergeant

<p align="center">
  <img width="40%" src="assets/sergeant.png" />
</p>

A Slack bot that tracks who owes you money.

## Usage

```text
@sergeant @jan +20 PLN   Add 20 PLN to Jan's tab
@sergeant @jan -5 PLN    Subtract 5 PLN
@sergeant @jan reset     Clear your tab with Jan
@sergeant status         Show your balances
@sergeant pay set        Save a payment method
@sergeant help           Show all commands
```

For batch updates, put each person and amount on a separate line. In a DM, omit `@sergeant`. Status and payment replies appear where you ask.

## Setup

1. Give the Slack bot the `app_mentions:read`, `chat:write`, `chat:write.public`, `im:history`, and `reactions:write` scopes.
2. Subscribe to `app_mention` and `message.im`. Point Event Subscriptions and Interactivity at `https://your-host/slack/events`.
3. Copy [.env.example](.env.example) to `.env` and fill in `SLACK_BOT_TOKEN` and `SLACK_SIGNING_SECRET`.
4. Run `direnv allow` to load the Nix environment and `.env`, then `make run`. The default port is `8080`.

Run `make check` for formatting, vet, lint, and tests.

The bot starts the read-only ledger dashboard on `0.0.0.0:8081`. Open `http://<bot-host>:8081/` to browse `SERGEANT_LEDGER_DB_PATH`. To run the dashboard alone, use `go run ./cmd/sergeant ledger`.
The live view updates in place every two seconds. Browse older entries in pages of 32.
The header counts every journal entry. PLN and EUR turnover sum adjustments and resets once each, excluding opening balances.
