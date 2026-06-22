-- Reverse of 000_schema_bootstrap.up.sql.
--
-- Drops the dedicated schema and every plugin object inside it. Operator
-- runs this only when intentionally tearing down the v2 plugin store on
-- a shared tenant Postgres. Down migrations are NOT auto-applied by the
-- plugin (cmd/memory-plugin-postgres/main.go:runMigrations comment) —
-- this file is for manual operator-driven cleanup.

DROP SCHEMA IF EXISTS memory_plugin CASCADE;
