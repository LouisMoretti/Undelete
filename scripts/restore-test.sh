#!/bin/sh
# Test de restauration PostgreSQL de bout en bout : une sauvegarde jamais
# restaurée n'est pas une sauvegarde vérifiée. Ce script fabrique une base
# SOURCE jetable, y applique les migrations et des données synthétiques,
# produit une archive avec scripts/backup.sh, puis la restaure dans une base
# CIBLE explicitement DISTINCTE et VIERGE avant de comparer les deux.
#
# Sécurité opérationnelle -- ce script ne touche JAMAIS :
#   - à un volume Docker (aucun `docker volume rm|prune`, aucun volume nommé) ;
#   - à un conteneur qu'il n'a pas lui-même créé (noms uniques horodatés) ;
#   - à la base de dev ou de prod (il refuse de tourner si l'environnement
#     porte un DSN applicatif, cf. garde-fou plus bas) ;
#   - au répertoire ./backups du dépôt (l'archive part dans un mktemp -d).
# Le seul nettoyage effectué est `docker rm -f` sur SES deux conteneurs.
set -eu
# Pas de `set -o pipefail` ici, contrairement à scripts/backup.sh : celui-ci
# tourne dans le BusyBox ash de l'image postgres, qui le supporte, alors que ce
# script-ci tourne sur le /bin/sh de la machine hôte (dash sur Debian/Ubuntu),
# qui ne le supporte pas. Les étapes où l'échec d'un maillon serait masqué par
# le succès du suivant -- décompression puis rejeu SQL surtout -- sont donc
# écrites sans pipe : chaque commande est testée pour elle-même.

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
MIGRATIONS_DIR="$ROOT/bot/internal/storage/migrations"

# --- Garde-fou : refus d'un environnement ambigu -----------------------------
# La cible de restauration doit être une base jetable créée ici, jamais une
# base fournie par l'appelant. Si l'environnement porte déjà un DSN du projet,
# on ne peut pas exclure qu'il désigne la base de dev ou de prod : on s'arrête
# plutôt que de deviner.
for var in MIGRATION_DATABASE_URL DATABASE_URL; do
    eval "value=\${$var:-}"
    if [ -n "$value" ]; then
        cat >&2 <<EOF
restore-test: refus de tourner avec $var défini dans l'environnement.
Ce test crée lui-même ses deux bases jetables et n'accepte aucune cible
externe : une restauration dans une base existante l'écraserait.
Relancez dans un shell où $var n'est pas exporté (ex. \`env -u $var make test-restore\`).
EOF
        exit 1
    fi
done

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
    cat >&2 <<'EOF'
restore-test: le démon Docker est indisponible, aucun test n'a été exécuté.
Ce test a besoin de créer deux conteneurs PostgreSQL 16 jetables.
Aucun conteneur, réseau, image ou volume existant n'a été modifié.
EOF
    exit 1
fi

# --- Identité des ressources jetables ---------------------------------------
# Suffixe unique (PID + epoch) : deux exécutions concurrentes ou successives ne
# se marchent jamais dessus, et aucun nom ne peut entrer en collision avec un
# conteneur préexistant. Le script est donc rejouable tel quel.
suffix="$$-$(date -u +%s)"
src_container="undelete-restore-src-$suffix"
dst_container="undelete-restore-dst-$suffix"
src_db="undelete_restore_src"
dst_db="undelete_restore_dst"
admin_password="restore-test-throwaway"
app_password="restore-test-throwaway-app"

workdir=$(mktemp -d)
# L'archive est écrite depuis le conteneur (uid postgres) dans ce répertoire
# temporaire monté : il doit être inscriptible par cet uid. Répertoire créé à
# l'instant par mktemp et supprimé par le trap, jamais un chemin du dépôt.
chmod 0777 "$workdir"

cleanup() {
    docker rm -f "$src_container" >/dev/null 2>&1 || true
    docker rm -f "$dst_container" >/dev/null 2>&1 || true
    rm -rf "$workdir"
}
trap cleanup EXIT HUP INT TERM

failures=0
ok() { echo "  [OK]   $1"; }
ko() { echo "  [ECHEC] $1" >&2; failures=$((failures + 1)); }

# Compare une valeur attendue à une valeur observée et enregistre un verdict.
expect_eq() {
    label="$1"
    expected="$2"
    actual="$3"
    if [ "$expected" = "$actual" ]; then
        ok "$label : $actual"
    else
        ko "$label : attendu <$expected>, obtenu <$actual>"
    fi
}

# --- Démarrage d'un PostgreSQL 16 jetable ------------------------------------
# `--rm` + aucun `--volume` nommé : les données vivent dans la couche
# éphémère du conteneur et disparaissent avec lui. `--publish 127.0.0.1::5432`
# laisse Docker choisir un port libre, donc aucun conflit avec la stack locale.
start_pg() {
    name="$1"
    dbname="$2"
    docker run --detach --rm \
        --name "$name" \
        --publish 127.0.0.1::5432 \
        --env POSTGRES_USER=postgres \
        --env POSTGRES_PASSWORD="$admin_password" \
        --env POSTGRES_DB="$dbname" \
        --volume "$workdir:/work" \
        postgres:16-alpine >/dev/null

    # Attente sur 127.0.0.1 et non sur le socket Unix : l'entrypoint officiel
    # démarre d'abord un serveur temporaire accessible par socket seulement.
    # Un pg_isready sur socket réussirait donc avant l'écoute TCP réelle.
    attempt=0
    until docker exec "$name" pg_isready -h 127.0.0.1 -U postgres -d "$dbname" >/dev/null 2>&1; do
        attempt=$((attempt + 1))
        if [ "$attempt" -ge 60 ]; then
            docker logs "$name" >&2 || true
            echo "restore-test: PostgreSQL 16 ($name) n'est jamais devenu disponible" >&2
            exit 1
        fi
        sleep 1
    done
}

# psql non interactif dans un conteneur donné, base donnée. ON_ERROR_STOP est
# indispensable : sans lui psql continue après une erreur et sort en 0.
psql_in() {
    name="$1"
    dbname="$2"
    shift 2
    docker exec --interactive \
        --env PGPASSWORD="$admin_password" \
        "$name" \
        psql -v ON_ERROR_STOP=1 -h 127.0.0.1 -U postgres -d "$dbname" "$@"
}

# Requête scalaire : sortie brute, sans en-tête ni alignement.
query() {
    name="$1"
    dbname="$2"
    sql="$3"
    psql_in "$name" "$dbname" -tAc "$sql"
}

echo "restore-test: base SOURCE ($src_container / $src_db)"
start_pg "$src_container" "$src_db"

# Les migrations 0002 et 0003 posent des GRANT explicites sur le rôle
# applicatif : il doit exister avant de les rejouer. db/init n'est pas utilisé
# ici (ce test ne valide pas le provisioning, seulement la sauvegarde), donc le
# rôle est créé au minimum syndical. NOLOGIN suffit : personne ne s'y connecte
# pendant ce test. Les rôles ne sont pas dans un pg_dump de base (ce sont des
# objets globaux), la CIBLE doit donc le créer elle aussi avant restauration.
create_app_role() {
    psql_in "$1" "$2" -q -c \
        "DO \$\$ BEGIN
             IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'undelete_app') THEN
                 CREATE ROLE undelete_app NOLOGIN PASSWORD '$app_password';
             END IF;
         END \$\$;"
}
create_app_role "$src_container" "$src_db"

# --- Migrations : réplique fidèle du runner Go -------------------------------
# storage.RunMigrations (bot/internal/storage/db.go) crée schema_migrations
# avec CE DDL exact, trie les fichiers par nom (le préfixe numérique fixe donc
# l'ordre), saute les versions déjà présentes, et applique chaque migration
# AVEC son INSERT de version dans UNE SEULE transaction. On reproduit les
# quatre points ici : si ce script divergeait du runner, il validerait un
# schéma que le binaire ne produit jamais.
echo "restore-test: application des migrations (ordre numérique)"
psql_in "$src_container" "$src_db" -q -c \
    "CREATE TABLE IF NOT EXISTS schema_migrations (
         version    INT PRIMARY KEY,
         applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
     )"

expected_versions=""
for file in "$MIGRATIONS_DIR"/*.sql; do
    base=$(basename "$file")
    # "0001_init.sql" -> 1, comme parseVersion() côté Go.
    version=$(printf '%s' "${base%%_*}" | sed 's/^0*//')
    if [ -z "$version" ]; then
        echo "restore-test: nom de migration invalide: $base" >&2
        exit 1
    fi
    expected_versions="${expected_versions}${version}
"
    # Migration + enregistrement de sa version dans la même transaction :
    # une migration qui échoue à mi-chemin ne doit pas être marquée appliquée.
    # Passage par un fichier plutôt qu'un pipe pour que l'échec de la
    # construction du script soit visible (cf. absence de pipefail en tête).
    cp "$file" "$workdir/migration.sql"
    printf '\nINSERT INTO schema_migrations (version) VALUES (%s);\n' "$version" \
        >> "$workdir/migration.sql"
    psql_in "$src_container" "$src_db" -q --single-transaction < "$workdir/migration.sql"
    rm -f "$workdir/migration.sql"
    echo "  migration $version appliquée ($base)"
done
expected_versions=$(printf '%s' "$expected_versions" | sort -n)

# --- Données synthétiques ----------------------------------------------------
# Valeurs fictives et reconnaissables (préfixe RESTORE-TEST, identifiants
# Telegram hors plage réelle) : si elles fuitaient dans une base réelle, elles
# seraient immédiatement identifiables comme du jeu d'essai.
echo "restore-test: insertion des données synthétiques"
psql_in "$src_container" "$src_db" -q --single-transaction <<'SQL'
INSERT INTO users (telegram_user_id, retention_days) VALUES
    (999000001, 7),
    (999000002, 30);

INSERT INTO business_connections (id, owner_user_id, can_reply, is_enabled)
SELECT 'RESTORE-TEST-bc-1', id, TRUE, TRUE FROM users WHERE telegram_user_id = 999000001;

INSERT INTO chats (owner_user_id, business_connection_id, chat_id, title, username, type)
SELECT id, 'RESTORE-TEST-bc-1', -100999001, 'RESTORE-TEST chat', 'restore_test', 'private'
FROM users WHERE telegram_user_id = 999000001;

INSERT INTO messages (owner_user_id, business_connection_id, chat_id, message_id,
                      from_user_id, from_display, message_type, text_content, telegram_date)
SELECT id, 'RESTORE-TEST-bc-1', -100999001, 4242, 999000002, 'RESTORE-TEST expediteur',
       'text', 'RESTORE-TEST contenu canari', 1700000000
FROM users WHERE telegram_user_id = 999000001;

INSERT INTO notification_outbox (owner_user_id, owner_telegram_user_id, business_connection_id,
                                 chat_id, message_id, event_type, payload_text)
SELECT id, 999000001, 'RESTORE-TEST-bc-1', -100999001, 4242, 'deleted',
       'RESTORE-TEST payload canari'
FROM users WHERE telegram_user_id = 999000001;
SQL

# Empreintes de la SOURCE, relevées AVANT le dump : ce sont elles que la
# CIBLE devra reproduire à l'identique.
src_users=$(query "$src_container" "$src_db" "SELECT count(*) FROM users")
src_connections=$(query "$src_container" "$src_db" "SELECT count(*) FROM business_connections")
src_chats=$(query "$src_container" "$src_db" "SELECT count(*) FROM chats")
src_messages=$(query "$src_container" "$src_db" "SELECT count(*) FROM messages")
src_outbox=$(query "$src_container" "$src_db" "SELECT count(*) FROM notification_outbox")
src_canary=$(query "$src_container" "$src_db" \
    "SELECT text_content FROM messages WHERE chat_id = -100999001 AND message_id = 4242")

# --- Sauvegarde : le vrai scripts/backup.sh ----------------------------------
# On exécute le script de production lui-même, pas un pg_dump réécrit : c'est
# CE script dont on veut prouver que la sortie est restaurable. BACKUP_DIR
# pointe sur le mktemp -d monté, jamais sur le ./backups du dépôt.
echo "restore-test: sauvegarde via scripts/backup.sh"
# Copie (et non montage direct du dépôt) : le conteneur ne voit qu'un
# répertoire temporaire, jamais l'arborescence du projet.
mkdir -p "$workdir/scripts"
cp "$ROOT/scripts/backup.sh" "$workdir/scripts/backup.sh"
docker exec \
    --env MIGRATION_DATABASE_URL="postgres://postgres:${admin_password}@127.0.0.1:5432/${src_db}?sslmode=disable" \
    --env BACKUP_DIR=/work/backups \
    --env BACKUP_RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-14}" \
    "$src_container" sh /work/scripts/backup.sh

archive=$(find "$workdir/backups" -name 'undelete-*.sql.gz' -type f | head -n 1)
if [ -z "$archive" ]; then
    echo "restore-test: aucune archive produite par backup.sh" >&2
    exit 1
fi
echo "restore-test: archive $(basename "$archive") ($(wc -c < "$archive") octets)"

echo "restore-test: vérifications de l'archive"
if gzip -t "$archive" 2>/dev/null; then
    ok "intégrité gzip de l'archive"
else
    ko "intégrité gzip de l'archive"
fi

# --- Restauration dans une CIBLE distincte et vierge -------------------------
# Second conteneur, second nom, seconde base : la restauration ne peut
# structurellement pas atterrir dans la source ni dans une base existante.
echo "restore-test: base CIBLE ($dst_container / $dst_db) -- distincte et vierge"
start_pg "$dst_container" "$dst_db"
create_app_role "$dst_container" "$dst_db"

# Preuve que la cible est bien vierge avant restauration : aucune table.
dst_tables_before=$(query "$dst_container" "$dst_db" \
    "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'")
expect_eq "cible vierge avant restauration (tables publiques)" "0" "$dst_tables_before"

echo "restore-test: restauration (gunzip puis psql)"
restore_started=$(date -u +%s)
gunzip -c "$archive" > "$workdir/restore.sql"
psql_in "$dst_container" "$dst_db" -q < "$workdir/restore.sql"
restore_ended=$(date -u +%s)
# RTO mesuré : durée de la seule restauration (décompression + rejeu SQL),
# hors démarrage du serveur. Reportée dans docs/backup-restore.md.
restore_seconds=$((restore_ended - restore_started))

# --- Vérifications post-restauration ----------------------------------------
echo "restore-test: vérifications post-restauration"

for table in users business_connections chats messages notification_outbox schema_migrations; do
    present=$(query "$dst_container" "$dst_db" \
        "SELECT count(*) FROM information_schema.tables
         WHERE table_schema = 'public' AND table_name = '$table'")
    expect_eq "table restaurée: $table" "1" "$present"
done

dst_versions=$(query "$dst_container" "$dst_db" \
    "SELECT version FROM schema_migrations ORDER BY version")
if [ "$expected_versions" = "$dst_versions" ]; then
    ok "schema_migrations à jour : versions $(printf '%s' "$dst_versions" | tr '\n' ' ')"
else
    ko "schema_migrations : attendu <$(printf '%s' "$expected_versions" | tr '\n' ' ')>, obtenu <$(printf '%s' "$dst_versions" | tr '\n' ' ')>"
fi

expect_eq "comptage users" "$src_users" \
    "$(query "$dst_container" "$dst_db" 'SELECT count(*) FROM users')"
expect_eq "comptage business_connections" "$src_connections" \
    "$(query "$dst_container" "$dst_db" 'SELECT count(*) FROM business_connections')"
expect_eq "comptage chats" "$src_chats" \
    "$(query "$dst_container" "$dst_db" 'SELECT count(*) FROM chats')"
expect_eq "comptage messages" "$src_messages" \
    "$(query "$dst_container" "$dst_db" 'SELECT count(*) FROM messages')"
expect_eq "comptage notification_outbox" "$src_outbox" \
    "$(query "$dst_container" "$dst_db" 'SELECT count(*) FROM notification_outbox')"

# Intégrité du contenu, pas seulement du cardinal : un dump tronqué peut
# restaurer le bon nombre de lignes avec des colonnes vides.
expect_eq "contenu canari du message restauré" "$src_canary" \
    "$(query "$dst_container" "$dst_db" \
        'SELECT text_content FROM messages WHERE chat_id = -100999001 AND message_id = 4242')"
expect_eq "libellé de chat restauré" "RESTORE-TEST chat" \
    "$(query "$dst_container" "$dst_db" \
        'SELECT title FROM chats WHERE chat_id = -100999001')"
expect_eq "payload outbox restauré" "RESTORE-TEST payload canari" \
    "$(query "$dst_container" "$dst_db" \
        'SELECT payload_text FROM notification_outbox WHERE message_id = 4242')"

# La RLS FORCE est une propriété du schéma : si le dump la perdait, la base
# restaurée serait ouverte à tous les tenants sans que les comptages bougent.
rls_forced=$(query "$dst_container" "$dst_db" \
    "SELECT count(*) FROM pg_class
     WHERE relname IN ('messages', 'notification_outbox', 'chats')
       AND relrowsecurity AND relforcerowsecurity")
expect_eq "FORCE ROW LEVEL SECURITY restauré (3 tables)" "3" "$rls_forced"

echo
echo "restore-test: RTO mesuré (restauration seule) : ${restore_seconds}s"
if [ "$failures" -eq 0 ]; then
    echo "restore-test: SUCCÈS -- la sauvegarde est restaurable et vérifiée."
    exit 0
fi
echo "restore-test: ECHEC -- $failures vérification(s) en défaut." >&2
exit 1
