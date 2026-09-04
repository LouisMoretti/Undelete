// Package messages manages the messages and chats tables, both protected by
// FORCE ROW LEVEL SECURITY. Every method in this package that touches them
// goes through storage.DB.InTenant: there is deliberately no path that would
// query them via the bare pool.
//
// chats only carries display labels (cf. migration 0003): no method here
// queries it to decide what to save or notify.
package messages

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/outbox"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// Record represents a message to save. Phase 1: text only.
//
// TODO Phase 2: add the fields needed for media (Telegram file_id, local
// path under ./media, mime_type) and propagate them to a separate
// media_files table (see the TODO in migration 0001).
type Record struct {
	BusinessConnectionID string
	ChatID               int64
	MessageID            int64
	FromUserID           *int64
	FromDisplay          string
	// MessageType is always "text" in Phase 1; the column already exists in
	// the database so no migration is needed when media arrives.
	MessageType  string
	TextContent  string
	TelegramDate int64
	// ChatTitle/ChatUsername/ChatType are the chat label carried by the
	// update. They are not messages columns: they feed the chats table,
	// upserted in the same transaction as the message (the only moment the
	// code sees the full Chat, cf. migration 0003).
	ChatTitle    string
	ChatUsername string
	ChatType     string
}

// DeletedRecord is what is returned on deletion: just enough to notify the
// owner without having to query the full row separately.
type DeletedRecord struct {
	ChatID       int64
	MessageID    int64
	FromUserID   *int64
	FromDisplay  string
	MessageType  string
	TextContent  string
	TelegramDate int64
}

// Repository provides access to the messages table, exclusively via InTenant.
type Repository struct {
	db *storage.DB
}

func NewRepository(db *storage.DB) *Repository {
	return &Repository{db: db}
}

// Save inserts or updates a message.
//
// ON CONFLICT DO UPDATE: Telegram may redeliver an already processed update
// (poller restart before offset confirmation, network retry); the upsert
// makes the operation idempotent on the key (owner_user_id,
// business_connection_id, chat_id, message_id).
//
// On edit (edited=true), only the LATEST version of the text is kept
// (requested behavior: no edit history in Phase 1) and edited_at is set. On
// a new record (edited=false) via a business_message that accidentally
// matches an existing conflict (double delivery), edited_at is not touched.
//
// Constraint #8: this method is called for EVERY received business_message /
// edited_business_message, with no condition on chat_id or on any user
// preference -- there is nowhere in this package a notion of an "enabled" or
// "selected" chat. The only thing that determines whether a message reaches
// this point is the access scope Telegram actually granted to the Business
// connection (filtered upstream by business.Service.Resolve, not here).
func (r *Repository) Save(ctx context.Context, ownerUserID int64, m Record, edited bool) error {
	return r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		// Chat label first, in the SAME transaction: this is here, and nowhere
		// else, that the full Chat is visible (the deleted_business_messages
		// update does not reliably carry it for already known chats). A contact
		// rename is therefore reflected at the next message, never
		// retroactively.
		//
		// Constraint #8: no condition on chat_id here either; this table
		// describes the chats seen, it selects none of them.
		if _, err := tx.Exec(ctx, `
			INSERT INTO chats (owner_user_id, business_connection_id, chat_id, title, username, type)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (owner_user_id, business_connection_id, chat_id)
			DO UPDATE SET
				-- An update that carries no label (Telegram omits optional
				-- Chat fields depending on the update type) must not WIPE the
				-- one already known: only replace with a non-empty value.
				title        = CASE WHEN EXCLUDED.title    <> '' THEN EXCLUDED.title    ELSE chats.title    END,
				username     = CASE WHEN EXCLUDED.username <> '' THEN EXCLUDED.username ELSE chats.username END,
				type         = CASE WHEN EXCLUDED.type     <> '' THEN EXCLUDED.type     ELSE chats.type     END,
				last_seen_at = now()
		`, ownerUserID, m.BusinessConnectionID, m.ChatID, m.ChatTitle, m.ChatUsername, m.ChatType); err != nil {
			return fmt.Errorf("chat label upsert: %w", err)
		}

		_, err := tx.Exec(ctx, `
			INSERT INTO messages (
				owner_user_id, business_connection_id, chat_id, message_id,
				from_user_id, from_display, message_type, text_content,
				telegram_date, edited_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, CASE WHEN $10 THEN now() ELSE NULL END)
			ON CONFLICT (owner_user_id, business_connection_id, chat_id, message_id)
			DO UPDATE SET
				text_content = EXCLUDED.text_content,
				message_type = EXCLUDED.message_type,
				from_user_id = EXCLUDED.from_user_id,
				from_display = EXCLUDED.from_display,
				edited_at    = CASE WHEN $10 THEN now() ELSE messages.edited_at END
		`,
			ownerUserID, m.BusinessConnectionID, m.ChatID, m.MessageID,
			m.FromUserID, m.FromDisplay, m.MessageType, m.TextContent,
			m.TelegramDate, edited,
		)
		if err != nil {
			return fmt.Errorf("message upsert: %w", err)
		}
		return nil
	})
}

// MarkDeleted sets deleted_at for each message_id in the batch, in a single
// transaction, and returns the messages actually found (and therefore
// restorable to the owner).
//
// message_ids is an array (constraint #6): we iterate over it via ANY($n),
// one query for the whole batch rather than one query per id.
//
// COALESCE(deleted_at, now()): idempotent if Telegram redelivers the same
// deleted_business_messages update (we do not want to overwrite an already
// set deleted_at with a later timestamp).
func (r *Repository) MarkDeleted(ctx context.Context, ownerUserID, ownerTelegramUserID int64, businessConnectionID string, chatID int64, messageIDs []int64) ([]DeletedRecord, error) {
	var found []DeletedRecord

	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		// One extra query, in the same transaction: the chat label only serves
		// to make the alert readable. Missing row = chat never seen since
		// migration 0003 (no backfill), the alert falls back to "chat <id>".
		var chatTitle, chatUsername string
		if err := tx.QueryRow(ctx, `
			SELECT title, username FROM chats
			WHERE business_connection_id = $1 AND chat_id = $2
		`, businessConnectionID, chatID).Scan(&chatTitle, &chatUsername); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("reading chat label: %w", err)
		}

		rows, err := tx.Query(ctx, `
			UPDATE messages
			SET deleted_at = COALESCE(deleted_at, now())
			WHERE business_connection_id = $1
			  AND chat_id = $2
			  AND message_id = ANY($3)
			RETURNING chat_id, message_id, from_user_id, COALESCE(from_display, ''),
			          message_type, COALESCE(text_content, ''), telegram_date
		`, businessConnectionID, chatID, messageIDs)
		if err != nil {
			return fmt.Errorf("updating deleted_at: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var d DeletedRecord
			if err := rows.Scan(&d.ChatID, &d.MessageID, &d.FromUserID, &d.FromDisplay,
				&d.MessageType, &d.TextContent, &d.TelegramDate); err != nil {
				return fmt.Errorf("reading deleted message: %w", err)
			}
			found = append(found, d)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()

		// Sorted by message_id: the outbox delivers the rows of one batch in
		// insertion order, so a deterministic order here is what makes a
		// multi-message deletion (an album is exactly that) arrive in the order
		// it was sent, rather than in the order PostgreSQL happened to return.
		sort.Slice(found, func(i, j int) bool { return found[i].MessageID < found[j].MessageID })

		// chunkCount tracks, per message, how many outbox chunks were written:
		// the media entry takes the next index, and chunk_index is part of the
		// anti-duplicate unique key.
		chunkCount := make(map[int64]int, len(found))
		for _, d := range found {
			// telegram.BuildDeletionMessageRequests is the single source of the
			// format and UTF-16 splitting of deletion alerts (the bot-api-10.3
			// fixtures are produced by this same path): the chunks written to
			// the outbox here are exactly what the worker will send, with no
			// parallel local rewording.
			alert := telegram.DeletionAlert{
				OwnerTelegramUserID: ownerTelegramUserID,
				ChatID:              d.ChatID,
				ChatTitle:           chatTitle,
				ChatUsername:        chatUsername,
				FromDisplay:         d.FromDisplay,
				FromUserID:          d.FromUserID,
				MessageType:         d.MessageType,
				TelegramDate:        d.TelegramDate,
				Content:             d.TextContent,
			}
			for chunkIndex, request := range telegram.BuildDeletionMessageRequests(alert) {
				if err := outbox.InsertTx(ctx, tx, ownerUserID, ownerTelegramUserID,
					businessConnectionID, d.ChatID, d.MessageID, outbox.EventDeletedMessage,
					chunkIndex, request.Text); err != nil {
					return err
				}
				chunkCount[d.MessageID] = chunkIndex + 1
			}
		}

		// Media entries LAST, in the same transaction: the text context of the
		// whole batch reaches the owner first (the outbox delivers a message's
		// chunks in order, and the batch in insertion order), then the files.
		return enqueueMediaAlerts(ctx, tx, mediaAlertScope{
			ownerUserID:          ownerUserID,
			ownerTelegramUserID:  ownerTelegramUserID,
			businessConnectionID: businessConnectionID,
			chatID:               chatID,
		}, found, chunkCount)
	})
	if err != nil {
		return nil, err
	}

	// message_ids of the batch missing from `found` correspond to messages
	// never seen (prior to the Business connection) or already purged by
	// retention: expected behavior, not an error. The debug level and the
	// "continue" decision are up to the caller (app/handler.go), which has the
	// context of the full batch.
	return found, nil
}

// PurgeExpired deletes messages whose retention has elapsed, tenant by
// tenant.
//
// Non-negotiable trap: this MUST NOT be a global DELETE executed via the bare
// pool. With FORCE ROW LEVEL SECURITY on messages and no
// app.current_owner_user_id context set, such a DELETE would run WITHOUT
// ERROR and delete EXACTLY ZERO rows (the USING policy filters everything,
// NULL never matching anything) -- the purge would appear to work
// indefinitely (no error log) while never purging anything. Hence the
// explicit tenant-by-tenant loop, each in its own InTenant.
func (r *Repository) PurgeExpired(ctx context.Context, tenants []users.TenantRetention) (int64, error) {
	var totalPurged int64

	for _, t := range tenants {
		err := r.db.InTenant(ctx, t.OwnerUserID, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				DELETE FROM messages
				WHERE owner_user_id = $1
				  AND saved_at < now() - make_interval(days => $2)
			`, t.OwnerUserID, t.RetentionDays)
			if err != nil {
				return fmt.Errorf("purge tenant %d: %w", t.OwnerUserID, err)
			}
			totalPurged += tag.RowsAffected()
			return nil
		})
		if err != nil {
			return totalPurged, err
		}
	}

	return totalPurged, nil
}
