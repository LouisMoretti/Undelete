-- Migration 0001 : schéma initial.
--
-- Appliquée avec le DSN propriétaire (POSTGRES_USER, superuser dans l'image
-- Postgres) via MIGRATION_DATABASE_URL. Le runtime applicatif n'utilise
-- jamais ce rôle : voir db/init/01-app-role.sh pour la création du rôle
-- restreint undelete_app.

-- users : une ligne par titulaire de compte Telegram Business connecté.
-- Racine de la relation tenant -> pas de RLS ici : la table est interrogée
-- directement (upsert par telegram_user_id) avant même qu'un contexte
-- "owner" ait un sens.
CREATE TABLE users (
    id               BIGSERIAL PRIMARY KEY,
    telegram_user_id BIGINT NOT NULL UNIQUE,
    retention_days   INT NOT NULL DEFAULT 7 CHECK (retention_days BETWEEN 1 AND 365),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- business_connections : une ligne par connexion Business Telegram.
-- id = business_connection_id fourni par Telegram (texte opaque, pas un
-- entier généré par nous).
--
-- Pas de RLS sur cette table : c'est la table de RÉSOLUTION, interrogée par
-- id de connexion *avant* de savoir qui est le owner (business/service.go
-- doit d'abord lire cette table pour obtenir owner_user_id, avant de pouvoir
-- poser app.current_owner_user_id). Une policy RLS ici créerait une
-- dépendance circulaire : il faudrait connaître l'owner pour lire la ligne
-- qui donne l'owner. Le petit volume de cette table (une ligne par
-- connexion Business, jamais de contenu utilisateur) rend ce risque
-- acceptable.
CREATE TABLE business_connections (
    id             TEXT PRIMARY KEY,
    owner_user_id  BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    can_reply      BOOLEAN NOT NULL,
    is_enabled     BOOLEAN NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_business_connections_owner ON business_connections (owner_user_id);

-- messages : le cœur du produit. Un message par ligne, sauvegardé dès sa
-- réception (business_message / edited_business_message) car l'événement de
-- suppression (deleted_business_messages) ne transporte PAS le contenu.
--
-- L'identité d'un message est le quadruplet (owner_user_id,
-- business_connection_id, chat_id, message_id) : message_id n'est unique
-- qu'à l'intérieur d'un chat donné, jamais globalement. L'utiliser seul
-- provoquerait des collisions entre conversations différentes.
CREATE TABLE messages (
    id                      BIGSERIAL PRIMARY KEY,
    owner_user_id           BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    business_connection_id  TEXT NOT NULL,
    chat_id                 BIGINT NOT NULL,
    message_id              BIGINT NOT NULL,
    from_user_id            BIGINT NULL,
    from_display            TEXT NULL,
    message_type            TEXT NOT NULL,
    -- TODO Phase 4 : remplacer text_content (TEXT en clair) par
    -- text_encrypted BYTEA, chiffré AES-256-GCM avec une clé par tenant,
    -- via une migration dédiée. Phase 1 reste volontairement en clair
    -- (contenu en clair en base, cf. périmètre de la tâche).
    text_content            TEXT NULL,
    telegram_date           BIGINT NOT NULL,
    saved_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    edited_at               TIMESTAMPTZ NULL,
    deleted_at              TIMESTAMPTZ NULL,
    UNIQUE (owner_user_id, business_connection_id, chat_id, message_id)
);

-- Index de lookup sur le quadruplet complet (hors owner_user_id qui est déjà
-- couvert par la contrainte UNIQUE ci-dessus, utilisée pour l'upsert
-- ON CONFLICT et pour retrouver un message à la suppression).
CREATE INDEX idx_messages_lookup
    ON messages (business_connection_id, chat_id, message_id);

-- Index pour la purge de rétention : balayage par owner puis par date de
-- sauvegarde.
CREATE INDEX idx_messages_owner_saved_at ON messages (owner_user_id, saved_at);

-- RLS sur messages, avec FORCE : ENABLE ROW LEVEL SECURITY seul n'est PAS
-- L'identité propriétaire de table contourne normalement RLS ; FORCE rend la
-- policy applicable à ce propriétaire lorsqu'il n'est pas superuser et n'a
-- pas BYPASSRLS. Les superusers et rôles BYPASSRLS contournent TOUJOURS RLS,
-- même avec FORCE : le runtime vérifie donc explicitement au démarrage qu'il
-- est connecté comme undelete_app, sans ces attributs (storage.NewPool).
-- Le rôle de migration superuser reste strictement réservé au boot.
ALTER TABLE messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE messages FORCE ROW LEVEL SECURITY;

-- current_setting(..., true) avec le second argument à true = "missing_ok" :
-- si app.current_owner_user_id n'a jamais été posé dans la transaction,
-- current_setting renvoie NULL au lieu de lever une erreur. NULLIF(..., '')
-- gère le cas où la variable existe mais est vide. owner_user_id = NULL ne
-- matche jamais aucune ligne (NULL n'est égal à rien, pas même à NULL) :
-- le comportement est donc fail-closed par construction. Sans ce ", true",
-- une requête sans contexte poserait lèverait une erreur au lieu de
-- silencieusement retourner zéro ligne -- ce qui serait presque acceptable
-- ici, mais ", true" est ce qui nous permet de distinguer "contexte non
-- posé" (comportement voulu : zéro ligne) d'une vraie erreur applicative.
CREATE POLICY tenant_isolation ON messages
    USING (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint)
    WITH CHECK (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint);

-- TODO Phase 2 : table media_files (chat_id, message_id, file_id Telegram,
-- chemin local sous ./media, mime_type) reliée à messages par le
-- quadruplet ; retention/purge à étendre pour supprimer les fichiers
-- correspondants sur disque, pas seulement les lignes.

-- TODO Phase 2+ : commandes RGPD (/delete_my_data, /privacy) -> fonctions
-- de suppression complète par owner_user_id, à documenter dans le README
-- (section confidentialité) en lien avec BACKUP_RETENTION_DAYS.
