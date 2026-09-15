-- Migration 0009: the application role loses the migration ledger, and the
-- 0001 tables get the explicit grants every later migration already carries.
--
-- db/init/01-app-role.sh grants DML on every table of the schema, present and
-- future, to the application role. That default also covers
-- schema_migrations, which only the migration runner (owner role, through
-- MIGRATION_DATABASE_URL) ever reads or writes: an application role able to
-- DELETE a ledger row would have the next boot replay that migration's DDL.
-- The ledger is revoked from it.
--
-- 0001 relied on the same defaults for its own tables, unlike 0002, 0003,
-- 0004 and 0006, which grant explicitly because a database whose owner role's
-- default privileges were never configured would otherwise leave the bot
-- without access. The grants below are the ones the defaults already give:
-- on a configured database this changes nothing.
REVOKE ALL ON schema_migrations FROM undelete_app;

GRANT SELECT, INSERT, UPDATE, DELETE ON users, business_connections, messages TO undelete_app;
GRANT USAGE, SELECT ON SEQUENCE users_id_seq, messages_id_seq TO undelete_app;
