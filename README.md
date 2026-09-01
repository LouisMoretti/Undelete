# undelete

Bot Telegram anti-delete connecté via **Telegram Business / Automatisation
d'échange** (bot business connecté — pas un bot de groupe classique, et
surtout pas un userbot MTProto).

Dès que le titulaire connecte le bot à son compte Telegram Business, le bot
sauvegarde automatiquement les messages de **toutes les conversations
privées auxquelles cette connexion Business lui donne accès**. Il n'y a
aucun sélecteur de conversations côté `undelete` : pas de liste de chats à
cocher, pas d'allowlist, pas de préférence par conversation. Quand Telegram
signale une suppression, le bot retrouve le contenu original en base
(sauvegardé au moment de la réception, car l'événement de suppression ne
transporte pas le contenu) et notifie le titulaire.

## Périmètre de cette phase (Phase 1)

Mono-tenant, messages texte uniquement, contenu en clair en base. Le schéma
est déjà multi-tenant et sous Row Level Security (RLS) pour préparer les
phases suivantes. Médias, chiffrement et commandes RGPD sont marqués
`// TODO Phase N` dans le code, non implémentés.

## Setup Telegram (3 étapes)

1. **Créer le bot** via [@BotFather](https://t.me/BotFather) : `/newbot`,
   récupérer le jeton (`TELEGRAM_BOT_TOKEN`).
2. **Activer Business Mode** dans BotFather : `/mybots` → sélectionner le
   bot → *Business Mode* → *Turn on*. **Sans cette étape, Telegram refuse
   toute tentative de connexion Business** — le bot n'apparaît simplement
   pas dans la liste des chatbots disponibles.
3. **Connecter le bot** depuis le compte Telegram du titulaire :
   *Réglages* → *Telegram Business* → *Chatbots* → sélectionner le bot.
   C'est cette étape qui déclenche l'update `business_connection` reçu par
   le bot.

> Note honnête : l'entrée *Telegram Business* dans les réglages a longtemps
> été réservée aux comptes Telegram Premium. La documentation MTProto
> actuelle indique que connecter un *bot* business ne nécessiterait plus
> Premium côté utilisateur connectant le bot — à vérifier vous-même sur un
> compte non-Premium réel avant de compter dessus en production, la
> documentation et le comportement réel de l'app divergent parfois.

## Démarrage

```bash
cp .env.example .env
# éditer .env : TELEGRAM_BOT_TOKEN, mots de passe Postgres, etc.

docker compose up --build -d
docker compose logs -f bot
```

Au boot, le binaire applique les migrations avec le DSN propriétaire
(`MIGRATION_DATABASE_URL`) puis ouvre le pool applicatif avec le DSN
restreint (`DATABASE_URL`, rôle `undelete_app`), avant de démarrer le
long-polling.

## Supervision (probes et métriques)

Le bot ouvre un serveur HTTP dédié sur `HEALTH_ADDR` (défaut `:9090`, valeur
vide = serveur désactivé). Ce port reste interne au réseau Docker : il n'est
pas publié sur l'hôte, comme Postgres.

| Route | Réponse |
| --- | --- |
| `GET /livez` | `200 {"status":"ok"}` dès que le processus sert du HTTP, sans dépendance externe. |
| `GET /readyz` | `200` si la base répond (ping, 2 s) **et** si le dernier `getUpdates` réussi date de moins de 90 s ; sinon `503 {"status":"degraded","checks":{...}}`. |
| `GET /metrics` | Exposition texte Prometheus. |

Le healthcheck Compose du service `bot` interroge `/livez` et non `/readyz` :
la liveness ne doit dépendre ni de Postgres ni de Telegram, sinon un incident
externe ferait redémarrer en boucle un bot par ailleurs sain.

Métriques exposées (compteurs, sauf la dernière qui est une jauge) :
`undelete_updates_total`, `undelete_update_errors_total`,
`undelete_outbox_retries_total`, `undelete_outbox_failed_total`,
`undelete_deletions_total`, `undelete_outbox_backlog`.

`undelete_outbox_failed_total` compte les alertes abandonnées définitivement
(4xx non rejouable, ou tentatives épuisées). Elles quittent
`undelete_outbox_backlog`, qui ne compte que `pending`/`processing` : sans ce
compteur, une vague d'échecs se lirait comme une simple décrue du backlog.

Aucune série n'a de label, et la liste des noms est écrite en dur : la
cardinalité est bornée par construction et aucun identifiant, nom, texte de
message ou jeton ne peut se retrouver dans un scrape. Les réponses `/readyz`
suivent la même règle : les checks dégradés sont décrits par des raisons
courtes et fixes (`unreachable`, `stale`, `no_successful_poll_yet`), jamais
par le message d'erreur PostgreSQL, qui contiendrait le DSN.
## Exploitation

Le déploiement homelab est entièrement manuel : **[`docs/runbook.md`](docs/runbook.md)**
est la procédure de référence (préflight, ordre backup → migration → rollout →
vérifications, rollback, rotation des secrets, recette staging), et la liste
fermée des actions destructives interdites sans confirmation explicite.

Avant tout déploiement :

```bash
sh scripts/preflight.sh
```

Contrôle en lecture seule des variables requises, des permissions de `.env`,
de l'espace disque, de la distinction des deux DSN, des rôles PostgreSQL
(`undelete_app` sans privilège de contournement RLS) et de la validité du
jeton Telegram via `getMe` — jeton jamais affiché. Sortie en code 1 si un
point échoue.

La procédure de restauration d'un dump est documentée à part dans
`docs/backup-restore.md` (livré par la PR #40, couche du dessous dans la stack — existe dès que la stack est mergée).

## Tests d’intégration PostgreSQL 16

La suite réelle (sans mocks) vérifie les migrations et leur réexécution, le
rôle runtime, les refus de rôles dangereux et de DDL, le fail-closed RLS,
l’isolation CRUD de deux tenants et `PurgeExpired` tenant par tenant.

```bash
make test-integration
```

La commande démarre un conteneur PostgreSQL 16 jetable, sans volume Docker,
puis supprime uniquement ce conteneur à la fin. Elle ne prune et ne modifie
aucune ressource Docker existante. Si Docker n’est pas accessible, elle échoue
explicitement et accepte à la place deux DSN vers une instance PostgreSQL 16
locale préparée avec `db/init/01-app-role.sh`. Ce mode externe refuse toute
opération tant que la base rapportée par `current_database()` ne s’appelle pas
exactement `undelete_integration` et que l’opt-in destructif littéral n’est pas
fourni :

```bash
POSTGRES_INTEGRATION_ADMIN_DSN='postgres://...' \
POSTGRES_INTEGRATION_RUNTIME_DSN='postgres://undelete_app:...' \
POSTGRES_INTEGRATION_ALLOW_DESTRUCTIVE=I_UNDERSTAND_THIS_WILL_DELETE_DATA \
make test-integration
```

La recette Docker positionne elle-même cet opt-in, uniquement pour son
conteneur éphémère et sa base dédiée.

Le script câble aussi `OUTBOX_TEST_DATABASE_URL` (DSN runtime) et
`OUTBOX_TEST_MIGRATION_DATABASE_URL` (DSN admin) vers la même base éphémère,
pour que les tests outbox de `bot/internal/outbox` — gates par ces variables et
sinon silencieusement ignorés — tournent dans la même exécution, après les
migrations posées par la suite `bot/integration`. Des valeurs fournies par
l’appelant sont respectées telles quelles.

## CI

`.github/workflows/ci.yml` s’exécute sur chaque `pull_request` et sur chaque
`push` vers `main`, avec deux jobs :

- **`lint + tests unitaires`** : `gofmt -l` (échec si sortie), `go vet ./...`,
  `go test ./...` sur le module `bot/`.
- **`intégration PostgreSQL 16`** : `make test-integration`, qui démarre son
  propre conteneur PostgreSQL 16 jetable sur le runner et exécute la suite
  d’intégration puis les tests outbox.

Aucun secret n’est utilisé, aucune base réelle n’est exposée : uniquement des
conteneurs éphémères avec des identifiants jetables. Le jeton `GITHUB_TOKEN`
est en `contents: read`.

**Protection de branche à activer côté GitHub** (*Settings → Branches → Add
branch ruleset* / *Add rule* sur `main`) — non automatisable depuis ce dépôt :

1. *Require a pull request before merging*.
2. *Require status checks to pass before merging*, puis sélectionner les deux
   checks `lint + tests unitaires` et `intégration PostgreSQL 16` (ils
   n’apparaissent dans la liste qu’après une première exécution du workflow).
3. *Require branches to be up to date before merging*.
4. Interdire les force-push sur `main`.

## Architecture

```
                    getUpdates (long polling, allowed_updates explicite)
                              │
                              ▼
                    ┌───────────────────┐
                    │  telegram.Poller  │  séquentiel, backoff, offset
                    └─────────┬─────────┘  avance même si le handler échoue
                              │ Update
                              ▼
                    ┌───────────────────┐
                    │   app.Handler     │  route par type d'update
                    └─────────┬─────────┘
                              │
          ┌───────────────────┼───────────────────┐
          ▼                   ▼                   ▼
  business.Service    messages.Repository   outbox.Worker
  (résolution :        (InTenant + RLS)      (lease + backoff,
  cache→DB→API)         deleted_at + outbox   sendMessage sans
                         atomiques)            business_connection_id)
                              │
                              ▼
                        PostgreSQL 16
  users / business_connections / chats / messages / notification_outbox
             (FORCE RLS sur le contenu et l'outbox par tenant)
```

- **`db/init/01-app-role.sh`** crée le rôle applicatif `undelete_app`
  (`NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS`) au premier démarrage
  du conteneur Postgres.
- **`storage.RunMigrations`** applique `internal/storage/migrations/*.sql`
  avec le DSN propriétaire, au boot, avant l'ouverture du pool applicatif.
- **`storage.DB.InTenant`** est le seul point d'entrée légitime vers les
  tables `chats`, `messages` et `notification_outbox` : il pose
  `app.current_owner_user_id` en `LOCAL` (scope transaction) avant toute
  requête.
- **Outbox durable** : `deleted_at` et les chunks de notification sont écrits
  dans la même transaction. Une contrainte unique absorbe les redélivrances.
  Le worker reprend les jobs `pending` ou les leases `processing` expirés,
  respecte `retry_after` sur 429 et applique un backoff exponentiel sur les
  erreurs réseau et 5xx. Les états `pending`, `processing`, `sent` et `failed`
  restent observables sans journaliser le payload.
- **Alertes lisibles** : chaque notification de suppression porte le chat (son
  libellé quand il est connu, son `chat_id` toujours), l'expéditeur, le type et
  la date d'envoi en UTC, avant le contenu restitué. Le libellé vient de la
  table `chats`, écrite à chaque message reçu — c'est un libellé d'AFFICHAGE,
  jamais un filtre (cf. piège n°8). Un chat sans libellé connu s'affiche
  « chat &lt;id&gt; ».

### Garanties de livraison des alertes

- **At-least-once, pas exactly-once.** Le worker envoie l'alerte à Telegram
  *puis* acquitte la ligne d'outbox. Un crash entre les deux laisse la ligne en
  `processing` ; son lease expire et un worker la reprend. **Une alerte déjà
  reçue peut donc être livrée une seconde fois.** Aucun dédoublonnage n'est
  fait côté Telegram : l'inverse (acquitter avant d'envoyer) échangerait ce
  doublon contre une alerte perdue, ce qui est inacceptable pour ce produit.
- **Ordre garanti au sein d'un message, pas entre messages.** Les chunks d'un
  même message supprimé (même `business_connection_id`, `chat_id`,
  `message_id`, `event_type`) partent dans l'ordre de `chunk_index` : `Claim`
  refuse un chunk tant qu'un chunk d'index inférieur n'est pas `sent` ou
  `failed`. **Aucun ordre n'est garanti ENTRE deux messages différents** — un
  message replanifié par un backoff peut arriver après un message supprimé
  plus tard. Les alertes portent l'identifiant du chat, jamais un numéro de
  séquence : ne pas s'appuyer sur leur ordre d'arrivée.
- **Horloge unique.** Échéances de retry et expirations de lease sont
  entièrement évaluées et écrites par PostgreSQL (`clock_timestamp()`). Une
  dérive entre l'horloge du bot et celle de la base ne peut ni masquer un job
  ni le rendre réclamable trop tôt.

## Les pièges (contraintes non négociables)

1. **`allowed_updates` explicite** dans `getUpdates` :
   `business_connection`, `business_message`, `edited_business_message`,
   `deleted_business_messages`. Sans ça, Telegram n'envoie rien du tout —
   aucune erreur, juste le silence.
2. **Deux rôles Postgres, deux DSN.** `config.Load()` refuse de démarrer si
   `DATABASE_URL == MIGRATION_DATABASE_URL` : sinon le bot tournerait avec
   les privilèges du rôle propriétaire et RLS serait décoratif.
3. **`FORCE ROW LEVEL SECURITY`** sur `messages`. `ENABLE` seul ne
   s'applique pas au propriétaire de la table. Pas de RLS sur
   `business_connections` (table de résolution, interrogée avant de
   connaître l'owner).
4. **`InTenant`** est le seul chemin vers `messages`. La purge de rétention
   ne peut pas être un `DELETE` global via le pool nu : avec `FORCE RLS` et
   aucun contexte posé, la requête réussirait et supprimerait zéro ligne,
   sans erreur. `PurgeExpired` boucle tenant par tenant.
5. **Traitement séquentiel des updates.** Un worker pool parallèle pourrait
   traiter une suppression avant le message correspondant. Le passage à
   l'échelle futur se fera par sharding sur `chat_id`, jamais par pool non
   ordonné.
6. **`message_ids` est un tableau.** Une suppression groupée arrive en un
   seul update `deleted_business_messages`.
7. **Les alertes partent DU bot**, sans `business_connection_id` : ce champ
   enverrait le message *en tant que* le titulaire, dans la conversation
   surveillée.
8. **Sauvegarde exhaustive et automatique.** Aucune table, commande,
   variable d'environnement ou condition ne permet de choisir quels chats
   enregistrer. `is_enabled` porte sur la connexion Business entière,
   jamais sur une conversation individuelle. La table `chats` ne fait
   exception à rien : elle stocke un libellé pour rendre les alertes
   lisibles, sans aucun drapeau d'activation, et n'est jamais consultée
   pour décider quoi sauvegarder ou notifier.

## Confidentialité

- Toute conversation privée exposée par la connexion Business active est
  sauvegardée intégralement (texte), sans opt-out par conversation.
- **Limite technique importante** : la Bot API ne donne au bot ni accès
  rétroactif à l'historique du compte, ni visibilité sur une conversation
  que Telegram ne lui expose pas explicitement via la connexion Business.
  `undelete` conserve exhaustivement ce que Telegram lui livre via cette
  connexion **à partir de son activation**, et rien au-delà — ni avant, ni
  en dehors du périmètre que Telegram décide de transmettre.
- Les logs (`log/slog`, JSON) ne contiennent jamais de contenu de message :
  identifiants, types et compteurs uniquement.
- Rétention configurable par utilisateur (`retention_days`, 1 à 365 jours),
  purgée quotidiennement.
- Les sauvegardes de base de données (`scripts/backup.sh`) ne couvrent pas
  `./media` (Phase 2). La durée de rétention des backups
  (`BACKUP_RETENTION_DAYS`) est, de fait, la durée de survie résiduelle des
  données après une future commande `/delete_my_data` : les lignes
  supprimées en base restent présentes dans les archives déjà écrites
  jusqu'à leur propre purge. À documenter explicitement dans une future
  commande `/privacy`.

## Roadmap par phases

- **Phase 1 (cette tâche)** : mono-tenant, texte en clair, RLS en place.
- **Phase 2** : médias (table `media_files`, sauvegarde de `./media`
  séparément des dumps SQL), commandes RGPD (`/delete_my_data`,
  `/privacy`).
- **Phase 3** : multi-tenant réel (plusieurs titulaires simultanés, retrait
  du garde-fou `OWNER_TELEGRAM_USER_ID`).
- **Phase 4** : chiffrement du contenu (`text_encrypted BYTEA`,
  AES-256-GCM, clé par tenant) en remplacement de `text_content` en clair.
