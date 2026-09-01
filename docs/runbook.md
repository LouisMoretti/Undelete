# Runbook — déploiement, mise à jour et rollback

Exploitation d'`undelete` sur le homelab (Proxmox → VM NixOS → Docker Compose
lancé à la main). Pas de CI de déploiement : **chaque mise en production est
une exécution manuelle de cette procédure**, dans l'ordre.

Toutes les commandes se lancent depuis la racine du dépôt sur la VM.

> **Dépendances entre PR.** Ce runbook référence deux éléments livrés par
> d'autres PR de la même pile : les sondes HTTP `/livez`, `/readyz` et
> `/metrics` sur le port `9090` (**disponible après la PR probes, #6**) et
> `make test-restore` + `docs/backup-restore.md` (**disponible après la PR
> test-restore, #7**). Les étapes concernées sont marquées *(après #6)* /
> *(après #7)* et disposent d'une alternative applicable dès aujourd'hui.

---

## 0. Actions destructives — liste fermée

> ### ⛔ INTERDIT SANS CONFIRMATION EXPLICITE DE LOUIS
>
> Les commandes ci-dessous détruisent des données que **rien ne restaure**
> (les dumps de `./backups` couvrent la base, jamais le volume ni `./media`).
> Aucune n'est nécessaire pour déployer, mettre à jour ou rollbacker. Elles
> ne doivent **jamais** être lancées « pour débloquer » un incident, ni
> proposées comme remède par un agent.
>
> | Commande | Effet irréversible |
> |----------|--------------------|
> | `docker compose down -v` | supprime le volume `postgres_data` → **toute la base perdue** |
> | `docker volume rm undelete_postgres_data` | idem, sans même arrêter proprement |
> | `docker volume prune` / `docker system prune` | peut emporter `postgres_data` et d'autres volumes de la VM |
> | `DROP DATABASE` / `DROP SCHEMA` / `TRUNCATE` | vide la base sous le bot en cours d'exécution |
> | `psql < dump.sql` sur la base de production | écrase l'état courant (restauration : voir §4) |
> | `rm -rf ./backups` (ou suppression de dumps hors purge de rétention) | supprime le seul filet de sécurité |
>
> **Règle d'exploitation** : arrêter la stack se fait avec `make down`
> (= `docker compose down`, **sans `-v`**), qui préserve `postgres_data`.
> Toute purge d'espace disque se fait sur les *fichiers* de `./backups`
> (dumps les plus anciens), jamais sur des ressources Docker.

---

## 1. Préflight

### 1.1 Automatique

```bash
sh scripts/preflight.sh
```

Script en **lecture seule** (aucune écriture, aucune suppression), rejouable.
Il rapporte une ligne par vérification et sort en code 1 dès un `[ECHEC]` :

| Vérification | Détail |
|---|---|
| `.env` présent et chargé | parsé clé=valeur, jamais sourcé ; l'environnement l'emporte sur le fichier, comme docker compose |
| permissions de `.env` | attendu `600` (contient le jeton et les mots de passe Postgres) |
| variables requises | `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB`, `APP_DB_PASSWORD`, `MIGRATION_DATABASE_URL`, `DATABASE_URL`, `TELEGRAM_BOT_TOKEN` (cf. `.env.example`) |
| `OWNER_TELEGRAM_USER_ID` | garde-fou mono-tenant Phase 1 ; **échec si vide** hors dev local |
| `BACKUP_RETENTION_DAYS` | entier ; absent ⇒ `backup.sh` applique 14 jours |
| DSN distincts | `DATABASE_URL ≠ MIGRATION_DATABASE_URL`, même règle que `config.Load()` |
| espace disque | seuil `PREFLIGHT_MIN_DISK_GB` (défaut 2 Go) sur le FS du dépôt |
| `./backups` et `./media` | présents et inscriptibles (bind mounts du compose) |
| rôles PostgreSQL | rôle propriétaire joignable ; `undelete_app` existant, `NOSUPERUSER` et `NOBYPASSRLS` |
| jeton Telegram | `getMe` sur api.telegram.org ; **le jeton n'est jamais affiché**, toute sortie de l'API est masquée |

Un `[SKIP]` n'est pas bloquant : il signale une vérification **non faite**
(outil absent, base injoignable), à rejouer depuis un endroit qui le permet.

Seuil disque ajustable :

```bash
PREFLIGHT_MIN_DISK_GB=10 sh scripts/preflight.sh
```

**Rejouer le check des rôles depuis le réseau Docker.** Depuis la VM, les DSN
de `.env` pointent vers l'hôte `postgres` du réseau compose et ne résolvent
pas : le check sort en `[SKIP]`. Une fois la stack démarrée, il se rejoue
depuis le conteneur Postgres, qui embarque `psql` :

```bash
docker compose exec -T postgres \
  psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAX \
  -c "SELECT rolname, rolsuper, rolbypassrls FROM pg_roles WHERE rolname IN ('undelete_app', current_user)"
```

Attendu : `undelete_app|f|f`. Si `undelete_app` est absent, c'est que
`db/init/01-app-role.sh` n'a pas tourné — il ne s'exécute qu'au **premier**
démarrage du volume `postgres_data`, jamais sur un volume déjà initialisé.

### 1.2 Checklist manuelle

- [ ] `git status` propre et branche/tag attendus (`git log --oneline -1`).
- [ ] `.env` en `600`, propriétaire = utilisateur qui lance compose.
- [ ] Espace disque : `df -h .` — prévoir la base **plus** la rétention de dumps.
- [ ] `docker compose config` ne signale aucune variable non substituée.
- [ ] `make check` vert (build + vet + gofmt) sur le commit à déployer.
- [ ] Sauvegardes récentes présentes : `ls -lh backups/ | tail -5`.
- [ ] Bot toujours connecté côté Telegram (Business Mode actif, cf. README).
- [ ] Fenêtre de déploiement acceptée : pendant le rollout, les messages
      supprimés ne sont **pas** rattrapés rétroactivement (la Bot API ne
      rejoue pas l'historique) — les updates non consommés restent toutefois
      en file côté Telegram jusqu'à 24 h et sont traités au redémarrage.

---

## 2. Procédure de déploiement / mise à jour

**Ordre non négociable : backup → migration → rollout → vérifications.**

### Étape 1 — Backup

Le service `backup` du compose tourne en boucle (un dump immédiat puis toutes
les 24 h). Avant un déploiement, on force un dump frais **maintenant** :

```bash
docker compose exec -T backup sh /scripts/backup.sh
ls -lh backups/ | tail -3
```

Le script écrit `backups/undelete-<horodatage UTC>.sql.gz`, puis purge les
archives de plus de `BACKUP_RETENTION_DAYS` jours (fichiers uniquement). Un
`pg_dump` en échec ne laisse pas d'archive tronquée : le `trap` la supprime.

Si la stack est arrêtée, démarrer d'abord Postgres seul :

```bash
docker compose up -d postgres
docker compose exec -T backup sh /scripts/backup.sh
```

> **Ne pas déployer sans dump frais.** L'étape 2 applique des migrations avec
> le rôle propriétaire ; c'est le seul moment où le schéma peut changer de
> façon non réversible.

Noter le nom du dump : c'est le point de retour de la §4.

### Étape 2 — Migration

Les migrations ne se lancent pas séparément : `bot/cmd/bot/main.go` appelle
`storage.RunMigrations` avec `MIGRATION_DATABASE_URL` (rôle propriétaire)
**avant** d'ouvrir le pool applicatif (`DATABASE_URL`, rôle `undelete_app`,
sans droits DDL). Elles se jouent donc au boot de l'étape 3.

Avant de déployer, regarder ce qui va être appliqué :

```bash
ls bot/internal/storage/migrations/
git diff --stat <commit_déployé>..HEAD -- bot/internal/storage/migrations/
```

Une migration **destructive** (`DROP`, `ALTER ... DROP COLUMN`, `TRUNCATE`,
changement de type avec perte) impose de relire la §4 *avant* le rollout : le
rollback devient une restauration de dump, pas un simple retour d'image.

Le contrôle d'application se fait après l'étape 3, dans les logs JSON :

```bash
docker compose logs bot | grep '"msg":"migration appliquée"'
```

Chaque ligne porte `version` et `name`. Aucune ligne = aucune migration
nouvelle à appliquer (cas normal d'un déploiement sans changement de schéma).
Un échec de migration fait sortir le binaire en code 1 avec
`"msg":"arrêt sur erreur fatale"` : le pool applicatif n'est jamais ouvert,
donc le bot ne tourne **jamais** sur un schéma partiellement migré.

### Étape 3 — Rollout

```bash
git pull --ff-only
docker compose up --build -d      # équivalent : make up
docker compose ps
```

`--build` reconstruit l'image du bot depuis `bot/Dockerfile` (le compose bâtit
localement, il n'y a pas de registre : `docker compose pull` ne rapatrie que
`postgres:16-alpine`). `up -d` recrée uniquement les services dont la
définition ou l'image a changé ; Postgres n'est pas redémarré si rien ne le
concerne, et son volume est conservé dans tous les cas.

`docker compose ps` doit montrer `postgres` *healthy* et `bot` *running*. Le
bot attend `service_healthy` sur Postgres : un démarrage un peu long est
normal.

### Étape 4 — Vérifications

**a. Sondes HTTP** *(après #6)* — sur `:9090` :

```bash
curl -fsS http://localhost:9090/livez  && echo " livez OK"
curl -fsS http://localhost:9090/readyz && echo " readyz OK"
curl -fsS http://localhost:9090/metrics | head -20
```

`/livez` = processus vivant ; `/readyz` = migrations passées, pool applicatif
ouvert et poller démarré. Un `readyz` durablement rouge alors que `livez` est
vert ⇒ regarder la base avant de toucher au bot.

*Avant #6*, la vérification équivalente se lit dans les logs (ci-dessous) et
via `docker compose ps` (état `running`, pas de boucle de redémarrage :
`docker compose ps --format '{{.Name}} {{.Status}}'`).

**b. Logs** :

```bash
docker compose logs -f bot        # équivalent : make logs
```

Attendus au boot, dans l'ordre :

- `"msg":"migration appliquée"` (seulement s'il y en avait),
- `"msg":"démarrage du poller"` avec `allowed_updates` contenant les quatre
  types `business_connection`, `business_message`, `edited_business_message`,
  `deleted_business_messages`.

À surveiller : `"level":"ERROR"`, en particulier `outbox: échec traitement`
(alertes non délivrées) et `purge rétention: échec`. Les logs ne contiennent
jamais de contenu de message : identifiants, types et compteurs uniquement.

**c. Test synthétique bout-en-bout** — le seul contrôle qui prouve que la
chaîne complète fonctionne :

1. Depuis un second compte Telegram, envoyer un message dans une conversation
   privée couverte par la connexion Business.
2. Vérifier son enregistrement (compteur, sans lire le contenu).

   > **Piège RLS.** `messages`, `notification_outbox` et `chats` sont en
   > `FORCE ROW LEVEL SECURITY` : la policy s'applique **aussi au rôle
   > propriétaire**. Un `SELECT count(*) FROM messages` sans contexte posé
   > renvoie `0` **sans erreur** — un zéro qui ne veut rien dire. Il faut
   > poser `app.current_owner_user_id` dans la même transaction, avec
   > `users.id` (clé de substitution) et **non** le `telegram_user_id` :
   > `owner_user_id` référence `users(id)`.

   ```bash
   docker compose exec -T postgres \
     psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAX -c "
       SELECT set_config('app.current_owner_user_id',
                         (SELECT id::text FROM users
                          WHERE telegram_user_id = ${OWNER_TELEGRAM_USER_ID}), true);
       SELECT count(*) FROM messages WHERE saved_at > now() - interval '5 minutes';
     "
   ```

   Les deux instructions doivent rester dans **un seul** `-c` : `set_config`
   est posé en `LOCAL` (3ᵉ argument `true`) et ne survit pas à la transaction.
   Un `set_config` renvoyant une ligne vide signifie que l'utilisateur n'existe
   pas encore en base — c'est alors ça, le vrai résultat du test.
3. Supprimer ce message depuis le second compte.
4. **Attendre l'alerte du bot sur le compte titulaire** (quelques secondes :
   le worker d'outbox tourne à la seconde). L'alerte doit porter le chat,
   l'expéditeur, le type, la date UTC et le contenu restitué.
5. Vérifier qu'aucun job d'outbox ne reste bloqué (même contexte RLS que
   ci-dessus, `notification_outbox` est également en `FORCE RLS`) :
   ```bash
   docker compose exec -T postgres \
     psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAX -c "
       SELECT set_config('app.current_owner_user_id',
                         (SELECT id::text FROM users
                          WHERE telegram_user_id = ${OWNER_TELEGRAM_USER_ID}), true);
       SELECT status, count(*) FROM notification_outbox GROUP BY status;
     "
   ```
   `sent` attendu ; des `failed` ou des `processing` qui persistent signalent
   un problème d'envoi côté Telegram.

Une alerte reçue **deux fois** n'est pas un bug : la livraison est
at-least-once par conception (cf. README).

**Déploiement considéré comme réussi uniquement si a + b + c sont verts.**

---

## 3. Rollback

Stratégie inverse de la §2 : on revient d'abord au code, on ne touche à la
base qu'en dernier recours.

### 3.1 Retour au code précédent (cas par défaut)

```bash
git log --oneline -5                    # identifier le commit stable
git checkout <commit_stable>            # ou: git revert <commit_fautif> puis push
docker compose up --build -d
docker compose logs -f bot
```

Rejouer ensuite les vérifications §2 étape 4.

- **Incident détecté après merge** : préférer `git revert <commit_fautif>`
  (l'historique reste linéaire et poussable, aucun force-push — interdit).
- **Incident en cours, besoin d'un retour immédiat** : `git checkout
  <commit_stable>` sur la VM (état détaché), puis régulariser par un `revert`
  sur `main` à froid.
- **Image précédente encore présente** : `docker image ls | grep undelete`
  puis relancer le tag antérieur si le rebuild est trop lent. Ne pas
  supprimer d'images pendant un incident.

### 3.2 Quand NE PAS rollbacker la base

Dans **la grande majorité des cas, on ne restaure pas la base**. Les
migrations de ce projet sont additives (`CREATE TABLE`, `ADD COLUMN`,
contraintes) : une version antérieure du bot tourne sans problème sur un
schéma plus récent — les colonnes en trop sont simplement ignorées.

Restaurer la base ferait alors **perdre tous les messages capturés depuis le
dump**, c'est-à-dire exactement ce que le produit est censé protéger. Ne pas
restaurer si :

- la migration était additive (aucun `DROP` / `TRUNCATE` / perte de type) ;
- le bug est côté code, pas côté données ;
- l'incident est une indisponibilité (bot en boucle de crash) : le §3.1 suffit.

### 3.3 Restauration de la base (dernier recours)

**Uniquement si** une migration destructive a supprimé ou converti des
données, ou si la base est corrompue.

> ⛔ **Interdit sans confirmation explicite de Louis.** Une restauration
> écrase l'état courant et perd toute donnée postérieure au dump.

Procédure détaillée : **`docs/backup-restore.md` et `make test-restore`**
*(après #7)* — ce sont les références à suivre, y compris pour valider le
dump **avant** de l'appliquer.

Ordre imposé, quel que soit le chemin :

1. Arrêter le bot seul, laisser Postgres debout :
   `docker compose stop bot` (jamais `down -v`).
2. Prendre un dump de l'état **actuel**, même dégradé
   (`docker compose exec -T backup sh /scripts/backup.sh`) : il permet de
   revenir en arrière si la restauration se passe mal.
3. Restaurer le dump choisi dans une base **de vérification** d'abord, jamais
   directement en production (c'est ce qu'automatise `make test-restore`).
4. Confirmation explicite de Louis, puis restauration en production.
5. Redémarrer le bot (`docker compose up -d bot`) et rejouer les
   vérifications §2 étape 4, test synthétique compris.

---

## 4. Rotation des secrets

Aucune rotation ne nécessite de supprimer un volume. **`docker compose down -v`
et `docker volume rm/prune` restent interdits (§0).**

### 4.1 Jeton Telegram (`TELEGRAM_BOT_TOKEN`)

1. **Révoquer/régénérer** via [@BotFather](https://t.me/BotFather) :
   `/mybots` → le bot → *API Token* → *Revoke current token*. L'ancien jeton
   cesse de fonctionner **immédiatement** : à partir de cet instant le bot ne
   reçoit plus rien. Rotation à faire dans une fenêtre courte et assumée.
2. Mettre à jour `.env` (`TELEGRAM_BOT_TOKEN=`), garder `chmod 600 .env`.
3. Valider le nouveau jeton **avant** de redéployer :
   `sh scripts/preflight.sh` (le check `getMe` doit être `[ OK ]`, et le jeton
   n'apparaît dans aucune sortie).
4. Propager : `docker compose up -d bot` (recréation du conteneur — une simple
   `restart` ne relit pas `.env`).
5. Vérifier `"msg":"démarrage du poller"` dans les logs, puis refaire le test
   synthétique §2.4.c.
6. Vérifier que Business Mode est toujours actif et la connexion Business
   toujours en place côté compte titulaire (README, étapes 2 et 3).

### 4.2 Mot de passe du rôle applicatif (`APP_DB_PASSWORD`)

Ordre conçu pour **ne jamais se verrouiller dehors** : on change le mot de
passe côté serveur avec le rôle propriétaire (toujours joignable), puis on
aligne le DSN.

```bash
# 1. Rotation côté Postgres, avec le rôle propriétaire.
#    Saisir le nouveau mot de passe via \password : il n'apparaît ni dans
#    l'historique shell ni dans les logs Postgres (contrairement à un
#    ALTER ROLE ... PASSWORD '...' en clair).
docker compose exec postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
  -c "\password undelete_app"
```

2. Mettre à jour `.env` : `APP_DB_PASSWORD` **et** le mot de passe embarqué
   dans `DATABASE_URL` (les deux, sinon le bot ne se connecte plus).
3. `sh scripts/preflight.sh` (DSN toujours distincts, variables présentes).
4. `docker compose up -d bot`, puis vérifier l'absence d'erreur de connexion
   dans les logs et le test synthétique §2.4.c.

Le bot en cours d'exécution garde ses connexions ouvertes jusqu'à sa
recréation : la fenêtre d'indisponibilité se limite au redémarrage.
`db/init/01-app-role.sh` n'est **pas** rejoué (volume déjà initialisé) — la
rotation est purement un `ALTER ROLE`, pas une réinitialisation.

### 4.3 Mot de passe du rôle propriétaire (`POSTGRES_PASSWORD`)

Même logique, un cran plus délicat : ce rôle joue les migrations et les
backups.

1. `docker compose exec postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"
   -c "\password $POSTGRES_USER"`.
2. Mettre à jour `.env` : `POSTGRES_PASSWORD` **et** le mot de passe dans
   `MIGRATION_DATABASE_URL`.
3. `docker compose up -d bot backup` (le service `backup` utilise aussi
   `MIGRATION_DATABASE_URL` : oublier de le recréer casserait silencieusement
   les sauvegardes du lendemain).
4. Contrôle immédiat : `docker compose exec -T backup sh /scripts/backup.sh`
   doit produire une nouvelle archive.

> `POSTGRES_PASSWORD` dans le compose ne sert qu'à l'**initialisation** du
> volume. Le modifier dans `.env` ne change rien côté serveur sur un volume
> existant : le `ALTER ROLE` de l'étape 1 est la seule opération qui compte.
> Ne jamais « régler » une désynchronisation en recréant le volume (§0).

### 4.4 Après toute rotation

- [ ] `git status` : `.env` non suivi (il est dans `.gitignore`), aucun secret
      dans le diff.
- [ ] Ancien secret révoqué côté émetteur (BotFather), pas seulement remplacé
      dans `.env`.
- [ ] Sondes/logs verts et test synthétique repassé.

---

## 5. Recette staging

Il n'y a pas d'environnement de staging permanent. La recette s'appuie sur
l'infrastructure de test déjà présente, qui crée ses propres conteneurs
**éphémères** et ne touche à aucune ressource Docker existante.

### 5.1 Avant chaque déploiement

```bash
make check              # build + go vet + gofmt
make test-integration   # Postgres 16 jetable : migrations, RLS, isolation, outbox
```

`scripts/test-integration.sh` démarre un conteneur PostgreSQL 16 sans volume
et ne supprime que ce conteneur. Il refuse de travailler sur une base dont le
nom n'est pas exactement `undelete_integration` et exige l'opt-in destructif
littéral en mode externe (README) — deux garde-fous à ne jamais contourner
pour « tester plus vite » sur la base de prod.

### 5.2 Recette périodique

```bash
sh scripts/preflight.sh   # dérive de configuration, disque, validité du jeton
make test-restore         # (après #7) restauration d'un dump réel vers une base jetable
```

Un backup qui n'a jamais été restauré n'est pas un backup. `make test-restore`
est la vérification qui transforme `./backups` en filet de sécurité réel.

**Cron homelab suggéré** (utilisateur non-root propriétaire du dépôt) :

```cron
# Préflight quotidien : dérive de config, disque, jeton toujours valide.
15 6 * * *  cd /srv/undelete && sh scripts/preflight.sh >> /var/log/undelete-preflight.log 2>&1

# Recette hebdomadaire : suite d'intégration + restauration d'un dump réel.
30 3 * * 0  cd /srv/undelete && make test-integration >> /var/log/undelete-recette.log 2>&1
45 3 * * 0  cd /srv/undelete && make test-restore     >> /var/log/undelete-recette.log 2>&1
```

Sur NixOS, l'équivalent déclaratif (`services.cron.systemCronJobs` ou un
`systemd.timers`) est préférable à un `crontab -e` manuel. Adapter le chemin
`/srv/undelete` à l'emplacement réel du dépôt sur la VM.

---

## Aide-mémoire

| Besoin | Commande |
|---|---|
| Préflight | `sh scripts/preflight.sh` |
| Backup immédiat | `docker compose exec -T backup sh /scripts/backup.sh` |
| Déployer / mettre à jour | `make up` (`docker compose up --build -d`) |
| Logs | `make logs` (`docker compose logs -f bot`) |
| État des services | `docker compose ps` |
| Arrêter (volume préservé) | `make down` (`docker compose down`, **sans `-v`**) |
| Build + lint | `make check` |
| Tests d'intégration | `make test-integration` |
| Recette de restauration | `make test-restore` *(après #7)* |
| Sondes | `curl -fsS localhost:9090/{livez,readyz,metrics}` *(après #6)* |
