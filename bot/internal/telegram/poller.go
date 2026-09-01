package telegram

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/metrics"
)

const (
	pollTimeoutSeconds = 50 // sous le timeout HTTP client (voir NewClient)
	minBackoff         = time.Second
	maxBackoff         = time.Minute
)

// Handler traite un update. L'erreur retournée est loggée mais NE bloque
// jamais l'avancée de l'offset (voir Poller.Run) : un update empoisonné
// (handler qui échoue systématiquement) ne doit jamais figer le bot.
type Handler func(ctx context.Context, update Update) error

// Poller effectue le long polling getUpdates et livre les updates à un
// Handler de façon strictement séquentielle.
//
// Contrainte non négociable n°5 : Telegram livre les updates dans l'ordre
// d'émission. Un worker pool parallèle pourrait traiter un
// deleted_business_messages AVANT le business_message correspondant (deux
// goroutines concurrentes, ordre d'exécution non garanti) : la suppression
// ne retrouverait alors rien en base alors que le message existe bel et
// bien côté Telegram. Cette boucle reste donc volontairement séquentielle,
// un seul update traité à la fois. Le passage à l'échelle futur se fera par
// sharding sur chat_id (plusieurs pollers/handlers indépendants, chacun
// responsable d'un sous-ensemble de chats, ordre préservé À L'INTÉRIEUR de
// chaque shard), jamais par un pool de workers non ordonné sur un flux
// unique.
type Poller struct {
	client *Client
	logger *slog.Logger
	offset int64

	// lastSuccessUnixNano date le dernier getUpdates réussi. Écrit par la
	// boucle Run, lu par la probe de readiness depuis une autre goroutine :
	// d'où l'atomique, alors que offset reste un simple champ (jamais lu
	// hors de Run).
	lastSuccessUnixNano atomic.Int64
}

func NewPoller(client *Client, logger *slog.Logger) *Poller {
	return &Poller{client: client, logger: logger}
}

// LastSuccessfulPoll renvoie la date du dernier getUpdates réussi, ou la
// valeur zéro si aucun poll n'a encore abouti depuis le démarrage. Sert de
// signal de fraîcheur à la readiness (health.FreshnessSource).
func (p *Poller) LastSuccessfulPoll() time.Time {
	nanos := p.lastSuccessUnixNano.Load()
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

// Run boucle jusqu'à annulation du contexte.
func (p *Poller) Run(ctx context.Context, handle Handler) error {
	backoff := minBackoff

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		updates, err := p.client.GetUpdates(ctx, p.offset, pollTimeoutSeconds)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			wait := backoff
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.IsRateLimited() {
				// Respect strict de retry_after (429) : Telegram nous dit
				// exactement combien de temps attendre, on n'applique pas
				// notre propre backoff par-dessus.
				wait = time.Duration(apiErr.RetryAfter) * time.Second
			} else {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}

			metrics.AddUpdateErrors(1)
			p.logger.Error("échec getUpdates", slog.String("error", err.Error()), slog.Duration("wait", wait))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}

		backoff = minBackoff // succès : on réinitialise le backoff
		p.lastSuccessUnixNano.Store(time.Now().UnixNano())
		metrics.AddUpdates(int64(len(updates)))

		for _, u := range updates {
			if err := handle(ctx, u); err != nil {
				metrics.AddUpdateErrors(1)
				p.logger.Error("échec traitement update",
					slog.Int64("update_id", u.UpdateID),
					slog.String("error", err.Error()))
			}
			// L'offset avance MÊME SI le handler a échoué. Contrainte
			// explicite : si on ne faisait avancer l'offset qu'en cas de
			// succès, un update qui échoue systématiquement (bug de
			// traitement, contrainte DB violée, etc.) serait relivré à
			// l'identique à chaque poll et figerait le bot indéfiniment sur
			// ce seul update.
			p.offset = u.UpdateID + 1
		}
	}
}
