#!/bin/sh
# Préflight de déploiement : vérifie, SANS RIEN MODIFIER, que la machine et la
# configuration sont prêtes avant un `docker compose up`. Aucune écriture,
# aucune suppression, aucune commande destructive -- ce script est en lecture
# seule par construction et peut être relancé autant de fois que voulu.
#
# Usage :
#   sh scripts/preflight.sh
#
# Sortie : une ligne par vérification, préfixée [ OK ] / [ECHEC] / [SKIP].
# Code de retour 0 si aucun ECHEC, 1 sinon. Un [SKIP] (outil manquant, base
# injoignable depuis l'hôte) n'est jamais bloquant : il signale une
# vérification NON FAITE, à refaire depuis un endroit qui le permet.
#
# NOTE : le jeton Telegram n'est jamais affiché. Toute sortie externe
# (réponse de l'API) passe par masquer_jeton() avant impression.
set -eu

# Seuil d'espace disque libre, en gigaoctets (surchargeable).
PREFLIGHT_MIN_DISK_GB="${PREFLIGHT_MIN_DISK_GB:-2}"

# Racine du dépôt : le script est appelable depuis n'importe quel répertoire.
racine_depot="$(cd "$(dirname "$0")/.." && pwd)"

nb_echecs=0
nb_skips=0

ok() { echo "[ OK ] $1"; }
echec() { echo "[ECHEC] $1"; nb_echecs=$((nb_echecs + 1)); }
skip() { echo "[SKIP] $1"; nb_skips=$((nb_skips + 1)); }

# Remplace le jeton Telegram par une forme masquée dans un texte arbitraire.
# Appelé sur TOUT ce qui provient de l'API : un message d'erreur curl ou une
# réponse Telegram peuvent renvoyer l'URL appelée, jeton compris.
masquer_jeton() {
    if [ -n "${TELEGRAM_BOT_TOKEN:-}" ]; then
        sed "s|${TELEGRAM_BOT_TOKEN}|<TELEGRAM_BOT_TOKEN masqué>|g"
    else
        cat
    fi
}

echo "=== Préflight undelete ==="
echo "dépôt : ${racine_depot}"
echo

# --- 1. Chargement de .env -------------------------------------------------
# Le fichier .env n'est PAS un script shell (docker compose le lit comme une
# liste clé=valeur). On le parse ligne à ligne plutôt que de le sourcer : un
# `.` sur un fichier de secrets exécuterait tout ce qu'il contient.
#
# Précédence identique à docker compose : une variable déjà présente dans
# l'environnement l'emporte sur la valeur du fichier.
fichier_env="${racine_depot}/.env"
if [ -f "$fichier_env" ]; then
    while IFS= read -r ligne || [ -n "$ligne" ]; do
        case "$ligne" in
            ''|'#'*) continue ;;
            *=*) ;;
            *) continue ;;
        esac
        cle="${ligne%%=*}"
        valeur="${ligne#*=}"
        cle="${cle#"${cle%%[![:space:]]*}"}"
        cle="${cle%"${cle##*[![:space:]]}"}"
        case "$cle" in
            ''|*[!A-Za-z0-9_]*) continue ;;
        esac
        # Retire une éventuelle paire de guillemets englobants.
        case "$valeur" in
            \"*\") valeur="${valeur#\"}"; valeur="${valeur%\"}" ;;
            \'*\') valeur="${valeur#\'}"; valeur="${valeur%\'}" ;;
        esac
        # eval nécessaire pour lire la valeur courante d'un nom dynamique en
        # POSIX pur ; le nom est validé ci-dessus ([A-Za-z0-9_] uniquement).
        eval "actuelle=\${${cle}:-}"
        if [ -z "$actuelle" ]; then
            export "${cle}=${valeur}"
        fi
    done < "$fichier_env"
    ok ".env présent et chargé (${fichier_env})"

    # Permissions : .env contient le jeton du bot et les mots de passe
    # Postgres. Lisible par le groupe ou par tous = fuite silencieuse.
    mode=""
    if command -v stat >/dev/null 2>&1; then
        mode="$(stat -c '%a' "$fichier_env" 2>/dev/null || stat -f '%Lp' "$fichier_env" 2>/dev/null || echo '')"
    fi
    if [ -z "$mode" ]; then
        skip "permissions de .env : commande stat indisponible, à vérifier à la main (attendu 600)"
    else
        case "$mode" in
            600|400) ok "permissions de .env correctes (${mode})" ;;
            *) echec "permissions de .env trop larges (${mode}) : attendu 600 -- \`chmod 600 .env\`" ;;
        esac
    fi
else
    skip ".env absent : les variables sont lues depuis l'environnement courant"
fi

# --- 2. Variables requises -------------------------------------------------
# Calquées sur .env.example. Une variable vide compte comme absente.
for var in POSTGRES_USER POSTGRES_PASSWORD POSTGRES_DB APP_DB_PASSWORD \
           MIGRATION_DATABASE_URL DATABASE_URL TELEGRAM_BOT_TOKEN; do
    eval "valeur=\${${var}:-}"
    if [ -n "$valeur" ]; then
        ok "variable ${var} définie"
    else
        echec "variable ${var} manquante ou vide (cf. .env.example)"
    fi
done

# OWNER_TELEGRAM_USER_ID : facultatif au sens de config.Load(), mais c'est le
# garde-fou mono-tenant de la Phase 1. Vide = n'importe quel compte Telegram
# peut connecter le bot en Business. Bloquant hors développement local.
if [ -n "${OWNER_TELEGRAM_USER_ID:-}" ]; then
    case "$OWNER_TELEGRAM_USER_ID" in
        ''|*[!0-9]*) echec "OWNER_TELEGRAM_USER_ID doit être un entier (valeur non numérique)" ;;
        *) ok "OWNER_TELEGRAM_USER_ID défini (garde-fou mono-tenant actif)" ;;
    esac
else
    echec "OWNER_TELEGRAM_USER_ID vide : aucun garde-fou mono-tenant, toute connexion Business serait acceptée (acceptable en dev local UNIQUEMENT)"
fi

# BACKUP_RETENTION_DAYS a une valeur par défaut dans backup.sh (14) : absent
# n'est pas une erreur, mais une valeur non numérique casserait le `find -mtime`.
if [ -n "${BACKUP_RETENTION_DAYS:-}" ]; then
    case "$BACKUP_RETENTION_DAYS" in
        ''|*[!0-9]*) echec "BACKUP_RETENTION_DAYS doit être un entier (valeur non numérique)" ;;
        *) ok "BACKUP_RETENTION_DAYS=${BACKUP_RETENTION_DAYS} jours" ;;
    esac
else
    ok "BACKUP_RETENTION_DAYS non défini : backup.sh appliquera 14 jours"
fi

# --- 3. DSN applicatif != DSN propriétaire ---------------------------------
# Même règle que config.Load() : DSN identiques => le bot tournerait avec le
# rôle propriétaire et FORCE ROW LEVEL SECURITY deviendrait décoratif.
if [ -n "${DATABASE_URL:-}" ] && [ -n "${MIGRATION_DATABASE_URL:-}" ]; then
    if [ "$DATABASE_URL" = "$MIGRATION_DATABASE_URL" ]; then
        echec "DATABASE_URL et MIGRATION_DATABASE_URL sont identiques : le bot refusera de démarrer (RLS serait décoratif)"
    else
        ok "DATABASE_URL et MIGRATION_DATABASE_URL sont distincts"
    fi
else
    skip "comparaison des DSN impossible : au moins un des deux est manquant"
fi

# --- 4. Espace disque libre ------------------------------------------------
# Vérifié sur le système de fichiers du dépôt : c'est lui qui porte ./backups
# (dumps gzip) et, sauf configuration contraire, le volume postgres_data.
libre_ko="$(df -Pk "$racine_depot" 2>/dev/null | awk 'NR==2 {print $4}')"
if [ -z "$libre_ko" ]; then
    skip "espace disque : df n'a rien renvoyé pour ${racine_depot}"
else
    libre_go=$((libre_ko / 1024 / 1024))
    if [ "$libre_go" -ge "$PREFLIGHT_MIN_DISK_GB" ]; then
        ok "espace disque libre ${libre_go} Go (seuil ${PREFLIGHT_MIN_DISK_GB} Go)"
    else
        echec "espace disque libre ${libre_go} Go < seuil ${PREFLIGHT_MIN_DISK_GB} Go -- purger d'anciens dumps de ./backups, JAMAIS de volume Docker"
    fi
fi

# --- 5. Permissions des répertoires de données -----------------------------
# ./backups est un bind mount écrit par le service backup ; ./media est écrit
# par le bot, qui tourne en uid 10001 avec un rootfs en lecture seule.
for repertoire in backups media; do
    chemin="${racine_depot}/${repertoire}"
    if [ ! -d "$chemin" ]; then
        echec "répertoire ./${repertoire} absent : \`mkdir -p ${chemin}\`"
    elif [ -w "$chemin" ]; then
        ok "répertoire ./${repertoire} présent et inscriptible"
    else
        echec "répertoire ./${repertoire} présent mais non inscriptible"
    fi
done

# --- 6. Rôles PostgreSQL ---------------------------------------------------
# Vérifie que le rôle propriétaire répond et que le rôle applicatif existe
# sans privilège de contournement de RLS -- exactement ce que storage.NewPool
# revérifie au boot, mais AVANT de déployer.
#
# Depuis l'hôte, les DSN de .env pointent vers l'hôte `postgres` du réseau
# Docker et ne résolvent pas : l'échec de connexion est un SKIP explicite,
# pas un ECHEC. Rejouer alors le check depuis le réseau compose (voir
# docs/runbook.md).
if ! command -v psql >/dev/null 2>&1; then
    skip "rôles PostgreSQL : psql indisponible sur cette machine (voir docs/runbook.md pour le rejouer via docker compose)"
elif [ -z "${MIGRATION_DATABASE_URL:-}" ]; then
    skip "rôles PostgreSQL : MIGRATION_DATABASE_URL manquant"
else
    # Timeout court : un préflight ne doit jamais rester bloqué sur un hôte
    # injoignable.
    export PGCONNECT_TIMEOUT=5
    sortie_psql=""
    if sortie_psql="$(psql "$MIGRATION_DATABASE_URL" -tAX -c 'SELECT current_user' 2>&1)"; then
        ok "rôle propriétaire joignable (current_user=${sortie_psql})"

        attributs=""
        if attributs="$(psql "$MIGRATION_DATABASE_URL" -tAX \
            -c "SELECT rolsuper::text || ',' || rolbypassrls::text FROM pg_catalog.pg_roles WHERE rolname = 'undelete_app'" 2>&1)"; then
            case "$attributs" in
                "")
                    echec "rôle undelete_app absent : db/init/01-app-role.sh n'a pas tourné (il ne s'exécute qu'au PREMIER démarrage du volume postgres_data)"
                    ;;
                "false,false")
                    ok "rôle undelete_app présent, NOSUPERUSER et NOBYPASSRLS"
                    ;;
                *)
                    echec "rôle undelete_app présent mais privilégié (rolsuper,rolbypassrls = ${attributs}) : RLS contournable, le bot refusera de démarrer"
                    ;;
            esac
        else
            skip "attributs de undelete_app illisibles : ${attributs}"
        fi
    else
        skip "base injoignable depuis cette machine, rôles NON vérifiés : ${sortie_psql}"
    fi
fi

# --- 7. Jeton Telegram (getMe) ---------------------------------------------
# getMe est en lecture seule et ne consomme aucun update. Le jeton transite
# dans l'URL : ni l'URL ni la réponse brute ne sont imprimées telles quelles.
if [ -z "${TELEGRAM_BOT_TOKEN:-}" ]; then
    skip "jeton Telegram : TELEGRAM_BOT_TOKEN manquant, appel getMe non effectué"
elif ! command -v curl >/dev/null 2>&1; then
    skip "jeton Telegram : curl indisponible sur cette machine"
else
    reponse=""
    if reponse="$(curl -sS --max-time 10 \
        "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/getMe" 2>&1)"; then
        case "$reponse" in
            *'"ok":true'*)
                # Extraction du username sans jq (absent de l'image alpine).
                nom="$(printf '%s' "$reponse" | sed -n 's/.*"username":"\([^"]*\)".*/\1/p')"
                ok "jeton Telegram valide (getMe -> @${nom:-inconnu})"
                ;;
            *)
                detail="$(printf '%s' "$reponse" | masquer_jeton | head -c 200)"
                echec "jeton Telegram refusé par l'API : ${detail}"
                ;;
        esac
    else
        detail="$(printf '%s' "$reponse" | masquer_jeton | head -c 200)"
        skip "appel getMe impossible (réseau ?) : ${detail}"
    fi
fi

# --- Bilan -----------------------------------------------------------------
echo
echo "=== Bilan : ${nb_echecs} échec(s), ${nb_skips} vérification(s) non faite(s) ==="
if [ "$nb_echecs" -gt 0 ]; then
    echo "Préflight EN ÉCHEC : corriger les points ci-dessus avant tout déploiement."
    exit 1
fi
if [ "$nb_skips" -gt 0 ]; then
    echo "Préflight OK, mais certaines vérifications n'ont pas pu être faites (voir [SKIP])."
fi
echo "Préflight OK."
