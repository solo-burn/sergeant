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
4. Run `direnv allow` to load the Nix environment and `.env`, then `make run`. The bot listens on port `8081`.

Run `make check` for formatting, vet, lint, and tests.

The bot serves the read-only ledger dashboard at `http://<bot-host>:8082/` by default. Set `SERGEANT_DASHBOARD_ADDR` to change its listen address, for example `:9090`. Port `8081` is reserved for the bot.
New ledger entries appear automatically in the dashboard.
The header counts every journal entry. PLN and EUR turnover sum adjustments and resets once each, excluding opening balances.
