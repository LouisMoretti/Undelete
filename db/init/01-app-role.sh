#!/bin/sh
# Automatically executed by the official postgres image on first startup
# (docker-entrypoint-initdb.d), with the POSTGRES_USER role (superuser in the
# image).
#
# Creates the app role undelete_app, deliberately stripped of any elevated
# privilege: NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS. It is this role,
# and ONLY this one, that the bot uses via DATABASE_URL at runtime.
# The owner role (POSTGRES_USER) stays reserved for migrations, via
# MIGRATION_DATABASE_URL -- see config.Load() which refuses to start if both
# DSNs are identical.
set -e

: "${APP_DB_USER:?APP_DB_USER must be set}"
: "${APP_DB_PASSWORD:?APP_DB_PASSWORD must be set}"

# The psql variables are quoted according to their nature: :"app_user"
# produces a properly escaped SQL identifier, :'app_password' a SQL literal.
# Direct shell interpolation would break init with an apostrophe in the
# password and would allow injection via a trapped environment value.
psql -v ON_ERROR_STOP=1 \
    -v app_user="$APP_DB_USER" \
    -v app_password="$APP_DB_PASSWORD" \
    --username "$POSTGRES_USER" \
    --dbname "$POSTGRES_DB" <<-'EOSQL'
    SELECT format(
        'CREATE ROLE %I WITH LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS',
        :'app_user',
        :'app_password'
    )
    WHERE NOT EXISTS (
        SELECT FROM pg_catalog.pg_roles WHERE rolname = :'app_user'
    ) \gexec

    -- Minimal rights: read/write on the app tables, no DDL. The migrations
    -- (owner role) create the tables after this script at first boot; ALTER
    -- DEFAULT PRIVILEGES is therefore essential so that these future tables
    -- and sequences become accessible at runtime without granting schema
    -- ownership to the app role.
    GRANT USAGE ON SCHEMA public TO :"app_user";
    GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO :"app_user";
    GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO :"app_user";
    ALTER DEFAULT PRIVILEGES IN SCHEMA public
        GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO :"app_user";
    ALTER DEFAULT PRIVILEGES IN SCHEMA public
        GRANT USAGE, SELECT ON SEQUENCES TO :"app_user";
EOSQL
