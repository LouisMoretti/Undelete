#!/bin/sh
# Sauvegarde quotidienne de la base Postgres : pg_dump | gzip vers
# ./backups, puis purge des archives plus vieilles que BACKUP_RETENTION_DAYS.
#
# NOTE 1 : ce dump ne contient PAS ./media (répertoire des fichiers médias).
# En Phase 1 ce répertoire est vide (pas de gestion de médias), mais dès la
# Phase 2 il faudra une sauvegarde séparée de ./media -- pg_dump ne
# sauvegarde que la base de données, jamais le système de fichiers.
#
# NOTE 2 : BACKUP_RETENTION_DAYS est aussi, de fait, la durée de survie
# résiduelle des données d'un utilisateur après un futur /delete_my_data
# (Phase 2+) : supprimer les lignes en base ne supprime pas les backups déjà
# écrits sur disque, qui continueront de contenir ces données jusqu'à leur
# propre purge. Cette durée doit être documentée dans /privacy pour être
# honnête avec l'utilisateur sur ce que "suppression" signifie réellement.
set -eu
# BusyBox ash (image postgres:16-alpine) supporte pipefail. Sans lui, un
# pg_dump en échec serait masqué par le succès de gzip et produirait une
# archive vide présentée comme valide.
set -o pipefail

: "${MIGRATION_DATABASE_URL:?MIGRATION_DATABASE_URL doit être défini}"
BACKUP_DIR="${BACKUP_DIR:-./backups}"
BACKUP_RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-14}"

mkdir -p "$BACKUP_DIR"

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
dest="${BACKUP_DIR}/undelete-${timestamp}.sql.gz"

echo "backup: dump vers ${dest}"
# Supprime toute archive incomplète si pg_dump, gzip ou le conteneur échoue.
# Le trap est retiré seulement après la réussite de toute la pipeline.
trap 'rm -f "$dest"' EXIT HUP INT TERM
pg_dump "$MIGRATION_DATABASE_URL" | gzip > "$dest"
trap - EXIT HUP INT TERM

echo "backup: purge des archives de plus de ${BACKUP_RETENTION_DAYS} jours"
find "$BACKUP_DIR" -name 'undelete-*.sql.gz' -type f -mtime "+${BACKUP_RETENTION_DAYS}" -delete

echo "backup: terminé"
