package schema

import (
	"math"
	"strconv"
)

var Ledger = `
-- A transfer increases what the debtor owes the creditor. An adjustment in
-- the other direction uses the same representation with the two accounts swapped.
CREATE TABLE ledger_entries (
    id          INTEGER PRIMARY KEY CHECK (id > 0),
    kind        TEXT NOT NULL CHECK (kind IN ('opening', 'adjustment', 'reset')),
    creditor_id TEXT NOT NULL,
    debtor_id   TEXT NOT NULL CHECK (debtor_id <> creditor_id),
    currency    TEXT NOT NULL,
    amount_minor INTEGER NOT NULL CHECK (amount_minor > 0),
    recorded_at INTEGER NOT NULL DEFAULT (unixepoch()),
    actor_id    TEXT,
    command_id  TEXT,
    CHECK ((kind = 'opening' AND actor_id IS NULL AND command_id IS NULL)
        OR (kind <> 'opening' AND actor_id IS NOT NULL AND actor_id IN (creditor_id, debtor_id)))
) STRICT;

-- Existing balances are already reflected in debts. Seed the journal before
-- installing the trigger so opening entries cannot double those balances.
INSERT INTO ledger_entries(kind, creditor_id, debtor_id, currency, amount_minor, recorded_at)
SELECT 'opening', creditor_id, debtor_id, currency, amount_minor, updated_at
FROM debts WHERE amount_minor > 0;

CREATE VIEW account_postings AS
SELECT id AS entry_id, creditor_id AS account_id, debtor_id AS counterparty_id, currency, amount_minor
FROM ledger_entries
UNION ALL
SELECT id, debtor_id, creditor_id, currency, -amount_minor FROM ledger_entries;

CREATE TRIGGER ledger_no_update BEFORE UPDATE ON ledger_entries
BEGIN SELECT RAISE(ABORT, 'ledger entries are immutable'); END;

CREATE TRIGGER ledger_no_delete BEFORE DELETE ON ledger_entries
BEGIN SELECT RAISE(ABORT, 'ledger entries are immutable'); END;

CREATE TRIGGER ledger_no_replace BEFORE INSERT ON ledger_entries
WHEN EXISTS (SELECT 1 FROM ledger_entries WHERE id = NEW.id)
BEGIN SELECT RAISE(ABORT, 'ledger entries are immutable'); END;

-- Keep the current balance in the same transaction as its journal entry.
CREATE TRIGGER ledger_update_debts AFTER INSERT ON ledger_entries
BEGIN
    -- Apply only the part left after paying off any debt in the other direction.
    INSERT INTO debts(creditor_id, debtor_id, currency, amount_minor, inserted_at, updated_at)
    SELECT NEW.creditor_id, NEW.debtor_id, NEW.currency, NEW.amount_minor - opposite,
           NEW.recorded_at, NEW.recorded_at
    FROM (SELECT COALESCE((
        SELECT amount_minor FROM debts
        WHERE creditor_id = NEW.debtor_id AND debtor_id = NEW.creditor_id AND currency = NEW.currency
    ), 0) AS opposite)
    WHERE NEW.amount_minor > opposite
    ON CONFLICT(creditor_id, debtor_id, currency) DO UPDATE SET
        amount_minor = CASE
            WHEN debts.amount_minor > ` + strconv.FormatInt(math.MaxInt64, 10) + ` - excluded.amount_minor
            THEN RAISE(ABORT, 'balance exceeds the supported amount')
            ELSE debts.amount_minor + excluded.amount_minor END,
        updated_at = excluded.updated_at;

    DELETE FROM debts
    WHERE creditor_id = NEW.debtor_id AND debtor_id = NEW.creditor_id AND currency = NEW.currency
      AND amount_minor <= NEW.amount_minor;

    UPDATE debts SET amount_minor = amount_minor - NEW.amount_minor, updated_at = NEW.recorded_at
    WHERE creditor_id = NEW.debtor_id AND debtor_id = NEW.creditor_id AND currency = NEW.currency;
END;
`
