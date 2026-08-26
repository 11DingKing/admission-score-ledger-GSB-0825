-- 001_init.down.sql - Drop admission score ledger schema

DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS current_snapshots;
DROP TABLE IF EXISTS submissions;
