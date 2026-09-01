# Fixtures Telegram Bot API

Ces payloads **entièrement synthétiques** figent les contrats utilisés par le
client HTTP minimal. Ils ne proviennent d'aucun compte, chat ou message réel et
ne contiennent ni jeton, ni identifiant, ni contenu personnel réel.

Version vérifiée : **Telegram Bot API 10.3 (24 août 2026)**, d'après la
[documentation officielle](https://core.telegram.org/bots/api) et son
[changelog](https://core.telegram.org/bots/api-changelog).

Le répertoire versionné `bot-api-10.3` couvre les quatre types d'updates
Business, `rights.can_reply`, `user_chat_id`, une suppression groupée, Unicode,
les champs optionnels et l'absence autorisée de `from`. S'y ajoutent trois
contrats de bord : le `can_reply` legacy (posé sur la connexion, sans bloc
`rights`), l'enveloppe d'erreur `429` avec `parameters.retry_after`, et le tout
premier `getUpdates`, dont l'`offset` 0 n'est pas sérialisé (`omitempty`).

## Convention de comparaison

Une seule, valable pour toutes les fixtures de requête : les octets bruts du
corps HTTP sont comparés à ceux du fichier avec `bytes.Equal`. La **seule**
normalisation appliquée est le retrait de l'unique `\n` terminal de stockage ;
toute autre terminaison (absente, doublée, CRLF) fait échouer le test. Aucun
espace ni aucune indentation n'est toléré ni nettoyé ailleurs — les fixtures de
requête sont donc volontairement sur une seule ligne compacte, exactement comme
`encoding/json` les produit.

Les helpers correspondants vivent dans le package
[`telegramtest`](../telegramtest), à part du package `telegram` pour rester
importables par les packages appelants.

## Qui exerce quoi

Les fixtures d'alerte ne sont pas comparées à des requêtes reconstruites par le
test : ce sont les **chemins de production** qui les produisent.

| Fixture | Exercée par |
| --- | --- |
| `get-updates-*.json` | `telegram.Client.GetUpdates` |
| `get-business-connection-*.json` | `telegram.Client.GetBusinessConnection` |
| `send-message-welcome-request.json` | `business.Service.notifyWelcome` (+ le builder, côté `telegram`) |
| `send-message-deletion-request.json` | `telegram.BuildDeletionMessageRequests`, seul producteur du format (les chunks écrits en outbox par `messages.Repository.MarkDeleted` sortent de ce builder) |
| `send-message-rate-limited-response.json` | `telegram.Client.SendMessage` (respect de `retry_after`) et `SendMessageOnce` |

Toute évolution du format d'une alerte impose donc de **régénérer** la fixture
correspondante depuis le builder de production (jamais de retouche à la main) :
sérialiser la requête produite avec `encoding/json` et écrire le résultat suivi
de l'unique newline LF de stockage. `send-message-deletion-request.json` a été
régénérée ainsi lors du passage au format enrichi (chat, expéditeur, type,
date). `send-message-welcome-request.json` est inchangée, le texte de bienvenue
n'ayant pas bougé.

## `send-message-ok-envelope.json` n'est pas un contrat

Ce fichier est un **stub de transport**, délibérément nommé ainsi : il vérifie
seulement que le client accepte une enveloppe Bot API `ok: true`. Son `result`
vide ne décrit **aucun** des deux scénarios d'alerte et n'a pas à correspondre
au chat ni au texte de la requête — `SendMessage` ignore `result`. Ne pas le
lire comme la réponse attendue d'un `sendMessage` donné, ni l'enrichir pour
« coller » à un scénario : ce serait une précision fictive.
