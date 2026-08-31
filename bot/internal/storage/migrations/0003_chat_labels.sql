-- Migration 0003 : libellés de chat, pour que les alertes de suppression
-- soient lisibles sans avoir à décoder un chat_id numérique.
--
-- Contrainte n°8 : cette table n'est PAS une table de sélection. Elle ne
-- contient aucun drapeau d'activation et n'est jamais consultée pour décider
-- si un message doit être sauvegardé ou notifié -- uniquement pour l'AFFICHAGE
-- du libellé dans l'alerte. Une ligne est écrite pour TOUS les chats vus, sans
-- exception ni filtre.
--
-- title porte le libellé d'affichage déjà calculé côté application (Title pour
-- les chats qui en ont un, prénom + nom pour les chats privés, que Telegram ne
-- décrit jamais par un titre). username et type sont conservés à part, le
-- format d'alerte les composant lui-même.
--
-- last_seen_at documente la fraîcheur du libellé (un contact renommé met à
-- jour sa ligne au message suivant) ; aucune purge ne s'y adosse en Phase 1.
CREATE TABLE chats (
    owner_user_id           BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    business_connection_id  TEXT NOT NULL,
    chat_id                 BIGINT NOT NULL,
    title                   TEXT NOT NULL DEFAULT '',
    username                TEXT NOT NULL DEFAULT '',
    type                    TEXT NOT NULL DEFAULT '',
    last_seen_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_user_id, business_connection_id, chat_id)
);

-- Même mécanique de RLS que messages et notification_outbox : un libellé de
-- chat est une donnée personnelle du tenant, il reçoit donc la même isolation
-- FORCE (ENABLE seul ne s'applique pas au propriétaire de la table).
--
-- Contrairement à business_connections, il n'y a AUCUNE circularité à faire
-- porter une policy ici : la ligne chats est écrite dans la même transaction
-- que le message correspondant, donc toujours après que
-- business.Service.Resolve a fourni owner_user_id et que InTenant a posé
-- app.current_owner_user_id. Sa lecture (alerte de suppression) a lieu dans la
-- transaction de MarkDeleted, où le contexte est posé pour les mêmes raisons.
-- Aucun chemin n'a besoin de lire chats pour découvrir un owner.
ALTER TABLE chats ENABLE ROW LEVEL SECURITY;
ALTER TABLE chats FORCE ROW LEVEL SECURITY;
CREATE POLICY chats_tenant_isolation ON chats
    USING (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint)
    WITH CHECK (owner_user_id = NULLIF(current_setting('app.current_owner_user_id', true), '')::bigint);

-- Grants explicites, comme en 0002 : cette migration peut s'exécuter dans une
-- base où les default privileges du rôle propriétaire n'ont pas été
-- configurés. Pas de séquence à accorder ici, la clé primaire est composite.
GRANT SELECT, INSERT, UPDATE, DELETE ON chats TO undelete_app;

-- Volontairement AUCUN backfill : les chats déjà connus de messages n'ont
-- jamais eu leur libellé persisté (l'update Telegram qui le portait est
-- passé), il n'y a donc rien à recopier. Ces chats restent sans ligne jusqu'à
-- leur prochain message, et l'alerte retombe alors sur le repli « chat <id> ».
