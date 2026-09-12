// Package erasure implements the /delete_my_data command: a single-use,
// expiring, non-replayable challenge, and the tenant-scoped deletion it
// authorises.
//
// # Two steps, because one is not an act
//
// /delete_my_data issues a code; /delete_my_data <code> spends it. A single
// command would make the most destructive action of the product a typo away,
// in a chat where the owner types all day. The code is random (crypto/rand),
// short-lived (ChallengeTTL) and spent atomically, so it cannot be guessed,
// cannot be reused, and cannot be quietly held for later.
//
// # The order of the deletion, and why it resumes
//
// The steps run in this order, and it is the order that makes an interrupted
// erasure safe to rerun rather than a state nobody can name:
//
//  1. disable the tenant's Business connections. Nothing new is captured from
//     this instant on, so every later step works on a set that only shrinks.
//     Doing it last would let the poller re-save, between two steps, a message
//     the owner just asked to have erased.
//  2. delete notification_outbox, every status included. An alert is content
//     on its way OUT of the system: the sooner the queue is empty, the smaller
//     the window in which a worker can deliver a message from a tenant that
//     asked to disappear.
//  3. delete the attachments: the blobs on disk first, then the catalogue rows
//     (media/purge owns that ordering and its rationale), then a sweep of the
//     tenant's own storage subtree for whatever an earlier interrupted attempt
//     left behind.
//  4. delete messages and chat labels, in one transaction.
//  5. delete the tenant's other erasure requests, and mark this one completed.
//
// Every step is idempotent by construction: a DELETE that matches nothing
// succeeds, an unlink of an absent file succeeds, and disabling an already
// disabled connection changes nothing. A crash therefore leaves a strict
// prefix of the list applied, and rerunning from step 1 re-applies it as a
// sequence of no-ops before continuing. No step needs anything a later step
// destroys -- the blobs are located from the catalogue rows, which is exactly
// why the rows go after the files and not before.
//
// The completed request row is the one thing the erasure deliberately keeps:
// it holds no content (an owner id, a hash, three timestamps) and it is what
// lets a replayed confirmation be answered with the same message instead of
// "unknown code" -- the difference between an idempotent command and one that
// accuses the owner of making it up.
package erasure

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ChallengeTTL is how long an issued code stays spendable.
//
// Ten minutes is long enough to read the message, think, and type the code
// back, and short enough that a code left on a screen, in a notification
// preview or in the chat history of the monitored conversation is not a
// standing authorisation to erase the account. The confirmation is typed in a
// chat the contact also reads (the Bot API delivers no other kind of message),
// so the window is the only thing bounding that exposure -- and a contact
// typing the code is refused anyway, since the answer only ever goes to the
// owner of the connection.
const ChallengeTTL = 10 * time.Minute

// codeAlphabet is the code alphabet: upper-case letters and digits, minus the
// four characters people confuse when reading a code off a screen (I, O, 0,
// 1). Exactly 32 symbols, which is what makes the byte-to-symbol mapping in
// newCode unbiased.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// codeLength is the number of symbols in a code. Eight symbols over a 32-symbol
// alphabet is 40 bits of entropy, against a window of ChallengeTTL, a single
// tenant, and a channel (Telegram messages the bot answers one at a time) that
// makes brute force visible long before it is feasible.
const codeLength = 8

// Outcome is what a submitted code did.
type Outcome int

const (
	// OutcomeErased: the code was valid and the deletion ran to its last step.
	// Also the outcome of resuming an erasure a crash interrupted.
	OutcomeErased Outcome = iota
	// OutcomeAlreadyErased: the code was already spent and its deletion
	// already completed. Nothing was deleted a second time.
	OutcomeAlreadyErased
	// OutcomeExpired: the code existed but its window closed.
	OutcomeExpired
	// OutcomeUnknown: no such code for this tenant. A code issued to another
	// tenant lands here too -- the lookup is tenant-scoped, and RLS makes that
	// true at the database level as well.
	OutcomeUnknown
)

// Tenant identifies who is asking. BusinessConnectionID is recorded on the
// request for traceability only: the erasure covers the OWNER, every
// connection of theirs included, never a single connection.
type Tenant struct {
	OwnerUserID          int64
	OwnerTelegramUserID  int64
	BusinessConnectionID string
}

// Challenge is an issued code and the instant it stops being spendable.
type Challenge struct {
	Code      string
	ExpiresAt time.Time
}

// ClaimState is what the store found when a code was submitted.
type ClaimState int

const (
	// ClaimUnknown: no row for this (tenant, code).
	ClaimUnknown ClaimState = iota
	// ClaimExpired: the row is still pending but its window closed.
	ClaimExpired
	// ClaimGranted: this caller won the row, moving it pending -> consumed.
	ClaimGranted
	// ClaimResumable: the row was already consumed and never completed. The
	// erasure it authorised is unfinished, so it is run again -- every step is
	// idempotent, and the data it covers was condemned the moment the code was
	// spent.
	ClaimResumable
	// ClaimCompleted: the row is completed. The deletion is done; a replay
	// deletes nothing.
	ClaimCompleted
)

// Challenges is the persistence of the challenge itself. An interface so the
// whole state machine above is exercised without a database: every branch of
// it is a decision about destroying data.
type Challenges interface {
	// Issue records a new pending code and invalidates the tenant's other
	// pending ones: one live code at a time, so an older message left in the
	// chat history cannot still authorise an erasure.
	Issue(ctx context.Context, t Tenant, codeHash string, ttl time.Duration) (time.Time, error)
	// Claim spends the code atomically and reports what it found.
	Claim(ctx context.Context, ownerUserID int64, codeHash string) (ClaimState, error)
	// Complete marks a consumed code completed.
	Complete(ctx context.Context, ownerUserID int64, codeHash string) error
	// DeleteOthers removes every request of the tenant except the given hash,
	// and returns how many went.
	DeleteOthers(ctx context.Context, ownerUserID int64, keepHash string) (int64, error)
}

// Connections disables the tenant's Business connections. Implemented by
// business.Service, which also holds the in-memory resolution cache: disabling
// in the database alone would leave the poller saving messages against a cached
// connection that still claims to be enabled.
type Connections interface {
	DisableOwner(ctx context.Context, ownerUserID int64) (int64, error)
}

// Outbox deletes the tenant's queued alerts, every status included.
type Outbox interface {
	DeleteTenant(ctx context.Context, ownerUserID int64) (int64, error)
}

// Media deletes the tenant's attachments: the blobs on disk and the catalogue
// rows that describe them.
type Media interface {
	EraseTenant(ctx context.Context, ownerUserID int64) (files int64, rows int64, err error)
}

// Messages deletes the tenant's messages and chat labels.
type Messages interface {
	DeleteTenant(ctx context.Context, ownerUserID int64) (messages int64, chats int64, err error)
}

// Service issues the challenges and runs the erasure they authorise.
type Service struct {
	challenges  Challenges
	connections Connections
	outbox      Outbox
	media       Media
	messages    Messages
	logger      *slog.Logger
}

// Config wires the Service. Every field is required: a Service missing one of
// them would report an erasure it did not perform, which is the one failure
// mode this command cannot have.
type Config struct {
	Challenges  Challenges
	Connections Connections
	Outbox      Outbox
	Media       Media
	Messages    Messages
	Logger      *slog.Logger
}

// New validates the configuration and returns the Service.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Challenges == nil:
		return nil, fmt.Errorf("erasure: Challenges is required")
	case cfg.Connections == nil:
		return nil, fmt.Errorf("erasure: Connections is required")
	case cfg.Outbox == nil:
		return nil, fmt.Errorf("erasure: Outbox is required")
	case cfg.Media == nil:
		return nil, fmt.Errorf("erasure: Media is required")
	case cfg.Messages == nil:
		return nil, fmt.Errorf("erasure: Messages is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(discardHandler{})
	}
	return &Service{
		challenges:  cfg.Challenges,
		connections: cfg.Connections,
		outbox:      cfg.Outbox,
		media:       cfg.Media,
		messages:    cfg.Messages,
		logger:      logger,
	}, nil
}

// Request issues a fresh code for the tenant. The code is returned in clear to
// the caller, which sends it to the owner and forgets it; only its hash is
// stored.
func (s *Service) Request(ctx context.Context, t Tenant) (Challenge, error) {
	code, err := newCode()
	if err != nil {
		return Challenge{}, err
	}
	expiresAt, err := s.challenges.Issue(ctx, t, HashCode(code), ChallengeTTL)
	if err != nil {
		return Challenge{}, err
	}
	// Ids and the expiry only: the code itself is never logged, and neither is
	// its hash -- a log line carrying either would turn the application log
	// into a live erasure token.
	s.logger.Info("erasure challenge issued",
		slog.Int64("owner_user_id", t.OwnerUserID),
		slog.String("business_connection_id", t.BusinessConnectionID),
		slog.Time("expires_at", expiresAt))
	return Challenge{Code: code, ExpiresAt: expiresAt}, nil
}

// Confirm spends a submitted code and, if it was valid, erases the tenant.
//
// An error means the erasure did not reach its last step. The request then
// stays 'consumed', and submitting the same code again resumes it from the
// beginning -- see the package comment for why rerunning is safe.
func (s *Service) Confirm(ctx context.Context, t Tenant, code string) (Outcome, error) {
	normalised := NormaliseCode(code)
	if normalised == "" {
		return OutcomeUnknown, nil
	}
	hash := HashCode(normalised)

	state, err := s.challenges.Claim(ctx, t.OwnerUserID, hash)
	if err != nil {
		return OutcomeUnknown, err
	}

	switch state {
	case ClaimUnknown:
		return OutcomeUnknown, nil
	case ClaimExpired:
		return OutcomeExpired, nil
	case ClaimCompleted:
		// Replay of a spent code whose erasure finished. Deliberately NOT a
		// second deletion and not an error: the owner gets the same answer
		// they got the first time.
		s.logger.Info("erasure confirmation replayed, nothing left to delete",
			slog.Int64("owner_user_id", t.OwnerUserID))
		return OutcomeAlreadyErased, nil
	case ClaimResumable:
		s.logger.Warn("resuming an interrupted erasure",
			slog.Int64("owner_user_id", t.OwnerUserID))
	}

	if err := s.erase(ctx, t, hash); err != nil {
		return OutcomeErased, err
	}
	if err := s.challenges.Complete(ctx, t.OwnerUserID, hash); err != nil {
		return OutcomeErased, err
	}
	return OutcomeErased, nil
}

// erase runs the five steps, in the order the package comment justifies. It
// stops at the first failure: a later step must never run on a tenant whose
// connections are still enabled, and what is already deleted stays deleted.
func (s *Service) erase(ctx context.Context, t Tenant, keepHash string) error {
	disabled, err := s.connections.DisableOwner(ctx, t.OwnerUserID)
	if err != nil {
		return fmt.Errorf("erasure: disabling connections: %w", err)
	}

	alerts, err := s.outbox.DeleteTenant(ctx, t.OwnerUserID)
	if err != nil {
		return fmt.Errorf("erasure: deleting queued alerts: %w", err)
	}

	files, mediaRows, err := s.media.EraseTenant(ctx, t.OwnerUserID)
	if err != nil {
		return fmt.Errorf("erasure: deleting attachments: %w", err)
	}

	deletedMessages, deletedChats, err := s.messages.DeleteTenant(ctx, t.OwnerUserID)
	if err != nil {
		return fmt.Errorf("erasure: deleting messages: %w", err)
	}

	// Every other erasure request of the tenant goes too -- the challenge is
	// tenant metadata like any other. The row being spent is the exception: it
	// is the receipt that makes a replayed confirmation answerable.
	requests, err := s.challenges.DeleteOthers(ctx, t.OwnerUserID, keepHash)
	if err != nil {
		return fmt.Errorf("erasure: deleting other erasure requests: %w", err)
	}

	// Counters only, as everywhere else: how much went, never what it was.
	s.logger.Info("tenant data erased",
		slog.Int64("owner_user_id", t.OwnerUserID),
		slog.Int64("connections_disabled", disabled),
		slog.Int64("alerts_deleted", alerts),
		slog.Int64("media_files_deleted", files),
		slog.Int64("media_rows_deleted", mediaRows),
		slog.Int64("messages_deleted", deletedMessages),
		slog.Int64("chats_deleted", deletedChats),
		slog.Int64("erasure_requests_deleted", requests))
	return nil
}

// HashCode is what the database stores: the SHA-256 of the normalised code,
// lowercase hex. The code itself never reaches a column, a log line or a
// backup -- a dump must not hand its reader a spendable erasure token.
func HashCode(code string) string {
	sum := sha256.Sum256([]byte(NormaliseCode(code)))
	return hex.EncodeToString(sum[:])
}

// NormaliseCode folds what a Telegram client may do to a code between the
// message that carries it and the message that returns it: case (mobile
// keyboards capitalise), surrounding whitespace, and the separators people
// insert when retyping by hand rather than copying.
func NormaliseCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(code)) {
		switch r {
		case ' ', '-', '_':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// newCode draws a code from crypto/rand.
//
// Each byte is mapped to one symbol modulo the alphabet size. That is unbiased
// here, and only here, because the alphabet has exactly 32 symbols and 256 is a
// whole multiple of 32: every symbol is the image of exactly 8 byte values.
// Changing the alphabet length without revisiting this is how a code generator
// quietly loses entropy.
func newCode() (string, error) {
	raw := make([]byte, codeLength)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("erasure: generating a confirmation code: %w", err)
	}
	out := make([]byte, codeLength)
	for i, b := range raw {
		out[i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	return string(out), nil
}

// discardHandler makes Config.Logger optional without a nil check on every
// call site (same pattern as media/purge and media/store).
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
