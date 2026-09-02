# Telegram Bot API Fixtures

These **fully synthetic** payloads pin down the contracts used by the minimal
HTTP client. They come from no real account, chat, or message and contain no
token, identifier, or real personal content.

Verified version: **Telegram Bot API 10.3 (August 24, 2026)**, per the
[official documentation](https://core.telegram.org/bots/api) and its
[changelog](https://core.telegram.org/bots/api-changelog).

The versioned `bot-api-10.3` directory covers the four Business update types,
`rights.can_reply`, `user_chat_id`, a bulk deletion, Unicode, optional fields,
and the permitted absence of `from`. Three edge contracts are added: the
legacy `can_reply` (set on the connection, without a `rights` block), the
`429` error envelope with `parameters.retry_after`, and the very first
`getUpdates`, whose `offset` 0 is not serialized (`omitempty`).

## Comparison convention

A single one, valid for all request fixtures: the raw bytes of the HTTP body
are compared to those of the file with `bytes.Equal`. The **only**
normalization applied is the removal of the single terminal storage `\n`; any
other termination (absent, doubled, CRLF) fails the test. No space or
indentation is tolerated or cleaned elsewhere — request fixtures are therefore
deliberately on a single compact line, exactly as `encoding/json` produces
them.

The corresponding helpers live in the
[`telegramtest`](../telegramtest) package, apart from package `telegram` so
they remain importable by the calling packages.

## Who exercises what

The alert fixtures are not compared to requests rebuilt by the test: it is the
**production paths** that produce them.

| Fixture | Exercised by |
| --- | --- |
| `get-updates-*.json` | `telegram.Client.GetUpdates` |
| `get-business-connection-*.json` | `telegram.Client.GetBusinessConnection` |
| `send-message-welcome-request.json` | `business.Service.notifyWelcome` (+ the builder, on the `telegram` side) |
| `send-message-deletion-request.json` | `telegram.BuildDeletionMessageRequests`, sole producer of the format (the chunks written to the outbox by `messages.Repository.MarkDeleted` come out of this builder) |
| `send-message-rate-limited-response.json` | `telegram.Client.SendMessage` (respect of `retry_after`) and `SendMessageOnce` |

Any change to an alert format therefore requires **regenerating** the
corresponding fixture from the production builder (never hand-editing):
serialize the produced request with `encoding/json` and write the result
followed by the single storage LF newline. `send-message-deletion-request.json`
was regenerated this way when moving to the enriched format (chat, sender,
type, date). `send-message-welcome-request.json` is unchanged, the welcome
text having not moved.

## `send-message-ok-envelope.json` is not a contract

This file is a **transport stub**, deliberately named so: it only verifies
that the client accepts a Bot API `ok: true` envelope. Its empty `result`
describes **none** of the two alert scenarios and does not have to match the
chat or the text of the request — `SendMessage` ignores `result`. Do not read
it as the expected response of a given `sendMessage`, nor enrich it to "fit"
a scenario: that would be fictitious precision.
