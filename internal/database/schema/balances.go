// Package schema defines the migrations used by the engines and the Jet generator.
package schema

const JournalVersion = 4

const Bootstrap = `
CREATE TABLE IF NOT EXISTS debts (
  creditor_id  TEXT    NOT NULL,
  debtor_id    TEXT    NOT NULL,
  currency     TEXT    NOT NULL,
  amount_minor INTEGER NOT NULL CHECK (amount_minor >= 0),
  inserted_at  INTEGER NOT NULL DEFAULT (unixepoch()),
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (creditor_id, debtor_id, currency)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS payment_methods (
  user_id     TEXT    NOT NULL,
  method      TEXT    NOT NULL,
  value       TEXT    NOT NULL,
  inserted_at INTEGER NOT NULL DEFAULT (unixepoch()),
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (user_id, method)
) WITHOUT ROWID;
`

var BalanceMigrations = []string{
	`ALTER TABLE payment_methods ADD COLUMN is_default INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE IF NOT EXISTS applied_commands (
		id TEXT PRIMARY KEY,
		reply TEXT NOT NULL,
		outcome TEXT NOT NULL
	) WITHOUT ROWID`,
	// Compute both directions from the original rows before updating either side.
	`WITH net AS MATERIALIZED (
		SELECT debt.creditor_id, debt.debtor_id, debt.currency,
		       MAX(0, debt.amount_minor - opposite.amount_minor) AS amount
		FROM debts AS debt
		JOIN debts AS opposite
		  ON opposite.creditor_id = debt.debtor_id
		 AND opposite.debtor_id = debt.creditor_id
		 AND opposite.currency = debt.currency
		WHERE debt.creditor_id <> debt.debtor_id
	)
	UPDATE debts SET amount_minor = net.amount
	FROM net
	WHERE debts.creditor_id = net.creditor_id
	  AND debts.debtor_id = net.debtor_id
	  AND debts.currency = net.currency;

	DELETE FROM debts WHERE amount_minor = 0;`,
}
