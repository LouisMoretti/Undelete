undelete — Privacy policy

Version: 1.1
Effective date: 2026-09-12

This document is the answer sent by the /privacy command, word for word.
It is the only copy: the command embeds this file, it does not paraphrase it.

1. WHAT IS SAVED

From the moment you connect undelete to your Telegram Business account, and
for as long as that connection stays enabled, the bot stores everything
Telegram delivers to it through that connection:

- the text of a message, or the caption of a media;
- who sent it (Telegram identifier and display name), in which chat (chat
  identifier, title or @username), and when;
- the attachments themselves, downloaded and kept as files on the server
  running this instance;
- the technical descriptors Telegram attaches to a file (type, size,
  dimensions, duration, file name, MIME type).

2. WHY

A Telegram deletion event carries the identifiers of the deleted messages and
nothing else — not one character of their content. Restoring what was deleted
is therefore only possible if the content was stored BEFORE the deletion. That
is the sole purpose of this storage; the data is used for nothing else.

3. AUTOMATIC AND WITHOUT SELECTION

There is no chat picker, no allowlist, no per-conversation opt-out, and no
setting that limits capture to some conversations. Every chat the Business
connection exposes is saved in full, automatically, including the messages of
the people writing to you. Enabling or disabling applies to the Business
connection as a whole, from your Telegram settings (Settings → Telegram
Business → Chatbots). Disabling it stops any further capture; it does not
delete what was already saved.

4. WHAT THE BOT CANNOT SEE (BOT API LIMITS)

undelete is a Telegram Business bot, not a userbot on your account. The Bot
API bounds it strictly:

- no retroactive access: nothing sent before the connection was established is
  reachable, and nothing is ever backfilled;
- no visibility outside what Telegram decides to expose through the Business
  connection: a chat Telegram does not forward simply does not exist for the
  bot;
- a deletion concerning a message the bot never received cannot be restored —
  the alert would have nothing to show;
- files above the Bot API ceiling of 20 MB cannot be downloaded at all: their
  existence is recorded, their bytes are not;
- a file reference can expire on Telegram's side before the download happens,
  in which case the media is lost even though its description remains.

5. WHO CAN READ IT

Alerts are only ever sent to the account holder, in a private conversation
with the bot, and never inside the monitored chat. The /privacy answer follows
the same rule: it is sent only to the holder of the Business connection, and a
third party asking for it receives nothing.

Beyond Telegram, the data lives in the PostgreSQL database and on the disk of
the server running this instance: whoever operates that server has technical
access to it. undelete is self-hosted software, not a service run by a third
party, and it sends nothing to anyone else.

6. LOGS

Application logs (JSON) contain identifiers, types, counters and durations
only. No message text, no caption, no file content is ever written to them.

7. HOW LONG IT IS KEPT

Messages, alerts and media are purged daily according to the retention period
of your account (retention_days, between 1 and 365 days, 7 by default). Once
that period has passed, the row is deleted and the file is unlinked from disk.
Retention is measured from the date the message was received, not from the
deletion.

8. SURVIVAL IN BACKUPS

Backups are the honest exception to the paragraph above. A deletion in the
database — retention purge or the /delete_my_data erasure command — only ever
affects live data. It cannot rewrite backup archives that were already written:
those keep their copy until they are purged in their own turn.

Database dumps are purged after BACKUP_RETENTION_DAYS days (14 by default),
which is therefore the real delay before the last trace of a message
disappears, not the retention period of section 7. Media archives are NOT
purged automatically: they survive until the operator deletes them.

As long as an archive exists, its copy is beyond the reach of any command
offered by the bot.

9. WHAT YOU CAN DO TODAY

- /privacy returns this document. The command is typed in a chat covered by
  the Business connection, because the Bot API never delivers a plain message
  sent to the bot: the person you are writing to sees that command in your
  conversation, and the bot saves it like any other message. The answer comes
  back to you alone, as a private message from the bot, split into several
  messages and labelled with their number.
- /delete_my_data erases everything this instance holds about you. It is typed
  in the same place and answered the same way as /privacy, and it takes two
  steps: typed alone it sends you a confirmation code, valid for a few minutes
  and usable once; typed followed by that code it disables your Business
  connections, then deletes your messages, your chat labels, your stored
  attachments (rows AND files on disk) and the alerts still queued. Submitting
  the same code again deletes nothing more and says so. A contact who types
  either form receives nothing and erases nothing: only the holder of the
  connection is answered.
  The confirmation code is sent to you privately, but the command that spends
  it is typed in a monitored chat, where your contact sees it like any other
  message — that is why it expires quickly and works only for you.
- Disabling the Business connection in your Telegram settings stops the
  capture immediately.
- Lowering retention_days shortens how long everything is kept.

An erasure covers the live data of this instance, and it is subject to section
8 like every other deletion: what an archive already holds stays there until
that archive is purged in its own turn. Reconnecting undelete afterwards starts
a new capture from zero.

10. CHANGES

Every substantive change to this policy bumps the version and the effective
date shown at the top of this document. Both are part of the repository and of
the bot's answer: they cannot drift apart.
