-- Read one snapshot. Zero differing rows means the journal reconstructs the debts table.
-- Entry order keeps each intermediate sum inside the supported balance range.
WITH reconstructed AS (
    SELECT account_id, counterparty_id, currency,
           SUM(amount_minor ORDER BY entry_id) AS balance
    FROM account_postings
    GROUP BY account_id, counterparty_id, currency
    HAVING balance > 0
), current AS (
    SELECT creditor_id, debtor_id, currency, amount_minor FROM debts
)
SELECT (SELECT count(*) FROM (SELECT * FROM reconstructed EXCEPT SELECT * FROM current))
     + (SELECT count(*) FROM (SELECT * FROM current EXCEPT SELECT * FROM reconstructed))
    AS differing_rows;
