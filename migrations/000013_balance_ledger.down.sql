DROP TRIGGER IF EXISTS trg_balance_ledger_append_only ON balance_ledger;
DROP TABLE IF EXISTS balance_ledger;
DROP FUNCTION IF EXISTS reject_mutation();
