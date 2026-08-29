#!/bin/sh
# Exécuté automatiquement par l'image postgres officielle au premier
# démarrage (docker-entrypoint-initdb.d), avec le rôle POSTGRES_USER
# (superuser dans l'image).
#
# Crée le rôle applicatif undelete_app, volontairement dépourvu de tout
# privilège élevé : NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS. C'est ce
# rôle, et UNIQUEMENT lui, que le bot utilise via DATABASE_URL en runtime.
# Le rôle propriétaire (POSTGRES_USER) reste réservé aux migrations, via
# MIGRATION_DATABASE_URL -- voir config.Load() qui refuse de démarrer si les
# deux DSN sont identiques.
set -e

: "${APP_DB_USER:?APP_DB_USER doit être défini}"
: "${APP_DB_PASSWORD:?APP_DB_PASSWORD doit être défini}"

# Les variables psql sont citées selon leur nature : :"app_user" produit un
# identifiant SQL correctement échappé, :'app_password' un littéral SQL. Une
# interpolation shell directe casserait l'init avec une apostrophe dans le mot
# de passe et permettrait une injection via une valeur d'environnement piégée.
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

    -- Droits minimaux : lecture/écriture sur les tables applicatives, pas de
    -- DDL. Les migrations (rôle propriétaire) créent les tables après ce
    -- script au premier boot ; ALTER DEFAULT PRIVILEGES est donc indispensable
    -- pour que ces futures tables et séquences deviennent accessibles au
    -- runtime sans accorder la propriété du schéma au rôle applicatif.
    GRANT USAGE ON SCHEMA public TO :"app_user";
    GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO :"app_user";
    GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO :"app_user";
    ALTER DEFAULT PRIVILEGES IN SCHEMA public
        GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO :"app_user";
    ALTER DEFAULT PRIVILEGES IN SCHEMA public
        GRANT USAGE, SELECT ON SEQUENCES TO :"app_user";
EOSQL
