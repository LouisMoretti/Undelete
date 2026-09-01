# Sauvegarde et restauration PostgreSQL

Une sauvegarde jamais restaurée n'est pas une sauvegarde vérifiée : c'est une
hypothèse. Ce document décrit la sauvegarde en place, l'objectif de perte de
données (RPO), l'objectif de temps de reprise (RTO), et la recette périodique
qui prouve que l'archive est réellement restaurable.

## Règle absolue

**Ne jamais supprimer ni purger un volume Docker existant.** Aucune procédure
de ce document n'appelle `docker volume rm`, `docker volume prune`,
`docker system prune` ni `docker compose down -v`. Le volume `pgdata` de la
stack contient la seule copie chaude des données : le détruire transforme un
incident en perte définitive. Une restauration se fait **toujours** vers une
cible neuve et explicitement distincte, jamais « par-dessus » l'existant.

## Ce qui est sauvegardé

`scripts/backup.sh` produit `backups/undelete-<horodatage UTC>.sql.gz` via
`pg_dump "$MIGRATION_DATABASE_URL" | gzip`, puis purge les archives de plus de
`BACKUP_RETENTION_DAYS` jours (14 par défaut).

Deux limites à connaître :

- **`./media` n'est pas dans le dump.** `pg_dump` ne sauvegarde que la base,
  jamais le système de fichiers. Dès la Phase 2 (gestion des médias), il faudra
  une sauvegarde séparée de ce répertoire.
- **Les rôles ne sont pas dans le dump.** `undelete_app` est un objet global du
  cluster, pas de la base. Une restauration dans un cluster neuf doit recréer ce
  rôle *avant* de rejouer l'archive, sinon les `GRANT` des migrations 0002 et
  0003 échouent. En exploitation, `db/init/01-app-role.sh` s'en charge à
  l'initialisation du cluster.

## RPO — perte de données maximale acceptée

| Paramètre | Valeur |
|---|---|
| Cadence de sauvegarde | quotidienne (`scripts/backup.sh`) |
| **RPO** | **24 h** — au pire, les messages reçus depuis le dernier dump réussi |
| Rétention des archives | `BACKUP_RETENTION_DAYS`, 14 jours par défaut |
| Profondeur de restauration | 14 jours ; au-delà, plus aucune archive |

Le RPO est directement la cadence : il n'y a ni archivage WAL ni réplication,
donc pas de restauration à un point dans le temps (PITR). Passer sous 24 h
demande de lancer `backup.sh` plus souvent — et augmente d'autant le nombre
d'archives à conserver.

La rétention a aussi une lecture « vie privée » : `BACKUP_RETENTION_DAYS` est
la durée de survie résiduelle des données d'un utilisateur après une
suppression en base, puisque les archives déjà écrites continuent de les
contenir jusqu'à leur propre purge.

## RTO — temps de reprise

Le RTO n'est pas estimé, il est **mesuré à chaque exécution de la recette** :
`scripts/restore-test.sh` chronomètre la restauration seule (décompression de
l'archive + rejeu SQL, `date -u +%s` avant et après) et l'affiche en fin de
sortie :

```
restore-test: RTO mesuré (restauration seule) : Ns
```

Ce que ce chiffre couvre et ne couvre pas :

- **couvert** : `gunzip` de l'archive puis rejeu complet du SQL sur une base
  vierge — le travail proportionnel au volume de données ;
- **non couvert** : provisionnement de la machine, `docker compose up`,
  démarrage de PostgreSQL, recréation du rôle applicatif. Comptez ces postes en
  plus dans le RTO opérationnel réel.

La granularité est la seconde ; sur un jeu de données de test, la mesure est
donc un plancher (`0s` ou `1s`) et non une projection. Elle devient
significative quand on la relève sur une archive de production : reportez la
valeur observée à chaque recette pour suivre sa dérive dans le temps.

## Recette de restauration automatisée

```sh
make test-restore
```

En un seul appel, `scripts/restore-test.sh` :

1. démarre un PostgreSQL 16 **source** jetable (`docker run --rm`, nom unique
   horodaté, port publié choisi par Docker, aucun volume nommé) ;
2. y applique `bot/internal/storage/migrations/*.sql` dans l'ordre numérique,
   en reproduisant fidèlement le runner Go (`storage.RunMigrations`) : même DDL
   pour `schema_migrations`, même tri par nom de fichier, migration et
   enregistrement de version dans une seule transaction ;
3. insère des données synthétiques reconnaissables (préfixe `RESTORE-TEST`,
   identifiants Telegram hors plage réelle) dans `users`,
   `business_connections`, `chats`, `messages` et `notification_outbox` ;
4. lance **le vrai `scripts/backup.sh`** — c'est bien la sortie du script de
   production qui est mise à l'épreuve — avec `BACKUP_DIR` dans un répertoire
   temporaire, jamais dans le `./backups` du dépôt ;
5. vérifie l'intégrité de l'archive avec `gzip -t` ;
6. démarre un second conteneur **cible**, distinct, avec une base vierge, et
   vérifie qu'elle est effectivement vide avant restauration ;
7. restaure (`gunzip` puis `psql`) en chronométrant l'opération ;
8. vérifie après restauration : présence des tables attendues (`users`,
   `business_connections`, `chats`, `messages`, `notification_outbox`,
   `schema_migrations`), versions de `schema_migrations` identiques à celles
   appliquées, comptages égaux à la source table par table, contenu des lignes
   canari (message, libellé de chat, payload d'outbox), et persistance de
   `FORCE ROW LEVEL SECURITY` sur les trois tables protégées ;
9. supprime, via un `trap`, **uniquement ses deux conteneurs** et son
   répertoire temporaire.

Sortie : un verdict `[OK]` / `[ECHEC]` par vérification, le RTO mesuré, et un
code de sortie non nul si la moindre vérification échoue.

Le script refuse de démarrer si `MIGRATION_DATABASE_URL` ou `DATABASE_URL` est
présent dans l'environnement : il crée lui-même ses deux bases jetables et
n'accepte aucune cible externe, pour qu'aucune restauration ne puisse atterrir
dans la base de dev ou de prod. Si ces variables sont exportées dans votre
shell :

```sh
env -u MIGRATION_DATABASE_URL -u DATABASE_URL make test-restore
```

Chaque exécution utilise des noms de conteneurs uniques (PID + horodatage) et
un `mktemp -d` neuf : la recette est rejouable telle quelle, y compris en
parallèle, sans nettoyage préalable.

## Cadence de la recette

**Hebdomadaire**, en local sur le homelab. Un test mensuel laisse trop de temps
à une régression silencieuse (migration ajoutée, changement d'image PostgreSQL,
archive tronquée) pour s'installer sans être vue.

Entrée `crontab -e` suggérée — dimanche 04:00, journal conservé pour pouvoir
relire le RTO mesuré :

```cron
0 4 * * 0 cd /chemin/vers/Undelete && env -u MIGRATION_DATABASE_URL -u DATABASE_URL make test-restore >> /var/log/undelete-restore-test.log 2>&1
```

À relire dans le journal après chaque passage : le verdict final et le RTO
mesuré. Un RTO qui grimpe régulièrement annonce le moment où la stratégie
« dump complet quotidien » ne suffira plus.

## Restauration réelle après incident

Procédure pas-à-pas, à faire **vers une cible neuve** :

1. **Choisir l'archive.** `ls -lt backups/undelete-*.sql.gz` — l'horodatage est
   en UTC. Vérifier son intégrité avant toute chose :
   `gzip -t backups/undelete-<horodatage>.sql.gz`.
2. **Préparer une cible vierge et distincte.** Un cluster neuf, ou au minimum
   une base nouvellement créée (`CREATE DATABASE undelete_restore;`) sur un
   cluster où `db/init/01-app-role.sh` a créé le rôle `undelete_app`. Ne jamais
   restaurer dans la base en service, et **ne jamais supprimer le volume
   existant** : tant que la restauration n'est pas validée, ce volume est la
   seule copie des données.
3. **Restaurer.**
   ```sh
   gunzip -c backups/undelete-<horodatage>.sql.gz \
     | psql "postgres://postgres:<motdepasse>@127.0.0.1:5432/undelete_restore"
   ```
   Utiliser `ON_ERROR_STOP` (`psql -v ON_ERROR_STOP=1`) : sans lui, `psql`
   poursuit après une erreur et sort en 0, ce qui présenterait une restauration
   partielle comme réussie.
4. **Vérifier avant de basculer.** Les mêmes contrôles que la recette :
   ```sql
   SELECT version FROM schema_migrations ORDER BY version;
   SELECT count(*) FROM users;
   SELECT count(*) FROM messages;
   SELECT relname, relrowsecurity, relforcerowsecurity
     FROM pg_class WHERE relname IN ('messages', 'notification_outbox', 'chats');
   ```
   `relforcerowsecurity` doit être vrai pour les trois tables : sans lui,
   l'isolation multi-tenant ne tient plus.
5. **Basculer.** Pointer `MIGRATION_DATABASE_URL` et `DATABASE_URL` sur la base
   restaurée, redémarrer le bot, et **seulement une fois le service vérifié**
   décider du sort de l'ancien volume. Le conserver au moins le temps d'un cycle
   de rétention reste le choix prudent.

Au redémarrage, le binaire rejoue ses migrations embarquées : les versions déjà
présentes dans `schema_migrations` sont sautées, une migration ajoutée depuis
l'archive est appliquée. Une restauration d'archive ancienne se remet donc
d'elle-même au niveau du schéma courant.
