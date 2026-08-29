# Fixtures Telegram Bot API

Ces payloads **entièrement synthétiques** figent les contrats utilisés par le
client HTTP minimal. Ils ne proviennent d'aucun compte, chat ou message réel et
ne contiennent ni jeton, ni identifiant, ni contenu personnel réel.

Version vérifiée : **Telegram Bot API 10.3 (24 août 2026)**, d'après la
[documentation officielle](https://core.telegram.org/bots/api) et son
[changelog](https://core.telegram.org/bots/api-changelog).

Le répertoire versionné `bot-api-10.3` couvre les quatre types d'updates
Business, `rights.can_reply`, `user_chat_id`, une suppression groupée, Unicode,
les champs optionnels et l'absence autorisée de `from`. Les requêtes attendues
pour `getUpdates`, `getBusinessConnection` et les deux catégories d'alertes
`sendMessage` sont comparées octet par octet par les tests.
