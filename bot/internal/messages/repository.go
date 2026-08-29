// Package messages gère la table messages, protégée par FORCE ROW LEVEL
// SECURITY. Toute méthode de ce package qui touche à messages passe par
// storage.DB.InTenant : il n'existe volontairement aucun chemin qui
// requêterait messages via le pool nu.
package messages

import (
	"context"
	"fmt"
	"unicode/utf16"

	"github.com/jackc/pgx/v5"

	"github.com/LouisMoretti/Undelete/bot/internal/outbox"
	"github.com/LouisMoretti/Undelete/bot/internal/storage"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// Record représente un message à sauvegarder. Phase 1 : texte uniquement.
//
// TODO Phase 2 : ajouter les champs nécessaires aux médias (file_id
// Telegram, chemin local sous ./media, mime_type) et les propager vers une
// table media_files séparée (voir TODO dans la migration 0001).
type Record struct {
	BusinessConnectionID string
	ChatID               int64
	MessageID            int64
	FromUserID           *int64
	FromDisplay          string
	// MessageType est Phase 1 toujours "text" ; le champ existe déjà en
	// base pour ne pas nécessiter de migration quand les médias arriveront.
	MessageType  string
	TextContent  string
	TelegramDate int64
}

// DeletedRecord est ce qui est restitué à la suppression : juste assez pour
// notifier le owner sans avoir à requêter la ligne complète séparément.
type DeletedRecord struct {
	ChatID      int64
	MessageID   int64
	FromDisplay string
	MessageType string
	TextContent string
}

// Repository donne accès à la table messages, exclusivement via InTenant.
type Repository struct {
	db *storage.DB
}

func NewRepository(db *storage.DB) *Repository {
	return &Repository{db: db}
}

// Save insère ou met à jour un message.
//
// ON CONFLICT DO UPDATE : Telegram peut relivrer un update déjà traité
// (redémarrage du poller avant confirmation de l'offset, retry réseau) ;
// l'upsert rend l'opération idempotente sur la clé (owner_user_id,
// business_connection_id, chat_id, message_id).
//
// Sur édition (edited=true), on ne garde que la DERNIÈRE version du texte
// (comportement demandé : pas d'historique des éditions en Phase 1) et on
// pose edited_at. Sur un nouvel enregistrement (edited=false) via un
// business_message qui matcherait par accident un conflit existant (double
// livraison), edited_at n'est pas touché.
//
// Contrainte n°8 : cette méthode est appelée pour TOUT business_message /
// edited_business_message reçu, sans aucune condition sur chat_id ou sur
// une quelconque préférence utilisateur -- il n'existe nulle part dans ce
// package de notion de chat "activé" ou "sélectionné". La seule chose qui
// détermine si un message arrive jusqu'ici est le périmètre d'accès que
// Telegram a effectivement transmis à la connexion Business (filtré en
// amont par business.Service.Resolve, pas ici).
func (r *Repository) Save(ctx context.Context, ownerUserID int64, m Record, edited bool) error {
	return r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
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
			return fmt.Errorf("upsert message: %w", err)
		}
		return nil
	})
}

// MarkDeleted positionne deleted_at pour chaque message_id du lot, dans une
// unique transaction, et renvoie les messages effectivement trouvés (donc
// restituables au owner).
//
// message_ids est un tableau (contrainte n°6) : on boucle dessus via
// ANY($n), une seule requête pour tout le lot plutôt qu'une requête par id.
//
// COALESCE(deleted_at, now()) : idempotent si Telegram relivre le même
// update deleted_business_messages (on ne veut pas écraser un deleted_at
// déjà posé par un timestamp plus tardif).
func (r *Repository) MarkDeleted(ctx context.Context, ownerUserID, ownerTelegramUserID int64, businessConnectionID string, chatID int64, messageIDs []int64) ([]DeletedRecord, error) {
	var found []DeletedRecord

	err := r.db.InTenant(ctx, ownerUserID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE messages
			SET deleted_at = COALESCE(deleted_at, now())
			WHERE business_connection_id = $1
			  AND chat_id = $2
			  AND message_id = ANY($3)
			RETURNING chat_id, message_id, from_display, message_type, text_content
		`, businessConnectionID, chatID, messageIDs)
		if err != nil {
			return fmt.Errorf("update deleted_at: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var d DeletedRecord
			if err := rows.Scan(&d.ChatID, &d.MessageID, &d.FromDisplay, &d.MessageType, &d.TextContent); err != nil {
				return fmt.Errorf("lecture message supprimé: %w", err)
			}
			found = append(found, d)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()

		for _, d := range found {
			text := fmt.Sprintf("Message supprimé récupéré (chat %d) :\n\n%s", d.ChatID, d.TextContent)
			for chunkIndex, chunk := range splitUTF16(text, 4096) {
				if err := outbox.InsertTx(ctx, tx, ownerUserID, ownerTelegramUserID,
					businessConnectionID, d.ChatID, d.MessageID, outbox.EventDeletedMessage,
					chunkIndex, chunk); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Les message_ids du lot absents de `found` correspondent à des
	// messages jamais vus (antérieurs à la connexion Business) ou déjà
	// purgés par la rétention : comportement attendu, pas une erreur. Le
	// niveau debug et la décision "on continue" sont du ressort de
	// l'appelant (app/handler.go), qui a le contexte du lot complet.
	return found, nil
}

func splitUTF16(text string, limit int) []string {
	var chunks []string
	current := make([]rune, 0, limit)
	units := 0
	for _, r := range text {
		runeUnits := utf16.RuneLen(r)
		if runeUnits < 1 {
			runeUnits = 1
		}
		if units+runeUnits > limit && len(current) > 0 {
			chunks = append(chunks, string(current))
			current = current[:0]
			units = 0
		}
		current = append(current, r)
		units += runeUnits
	}
	if len(current) > 0 {
		chunks = append(chunks, string(current))
	}
	return chunks
}

// PurgeExpired supprime les messages dont la rétention est dépassée,
// tenant par tenant.
//
// ⚠️ Piège non négociable : ceci ne PEUT PAS être un DELETE global exécuté
// via le pool nu. Avec FORCE ROW LEVEL SECURITY sur messages et aucun
// contexte app.current_owner_user_id posé, un tel DELETE s'exécuterait
// SANS ERREUR et supprimerait EXACTEMENT ZÉRO ligne (la policy USING filtre
// tout, NULL ne matchant jamais rien) -- la purge semblerait fonctionner
// indéfiniment (aucun log d'erreur) tout en ne purgeant jamais rien. D'où
// la boucle explicite tenant par tenant, chacun dans son propre InTenant.
func (r *Repository) PurgeExpired(ctx context.Context, tenants []users.TenantRetention) (int64, error) {
	var totalPurged int64

	for _, t := range tenants {
		err := r.db.InTenant(ctx, t.OwnerUserID, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				DELETE FROM messages
				WHERE owner_user_id = $1
				  AND saved_at < now() - ($2 || ' days')::interval
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
