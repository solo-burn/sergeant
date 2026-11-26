# Sergeant

Run `make check` inside `nix develop`.

- Keep `internal/legacy` and `internal/ledger` independent. Legacy answers Slack. Ledger replays committed commands in its own database.
- Implement account commands in both engines. Keep persisted command kinds stable and classify writes in `Command.Mutates`.
- Add user-facing text in English and Polish. Keep raw commands and payment details out of logs.
- After changing the SQLite schema, run `go generate ./internal/database` to refresh the Jet bindings.
