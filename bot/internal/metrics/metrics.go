// Package metrics expose des compteurs agrégés au format texte Prometheus.
//
// Règle non négociable : AUCUN contenu utilisateur n'entre ici. Ni
// telegram_user_id, ni chat_id, ni message_id, ni nom d'affichage, ni texte
// de message, ni jeton de bot ne doivent apparaître -- ni en valeur, ni en
// nom de série, ni surtout en LABEL. Une métrique labellisée par chat_id
// reconstituerait la liste des conversations surveillées dans n'importe quel
// scrape, et un endpoint /metrics est par nature moins protégé que la base.
//
// Corollaire volontaire : les séries exposées n'ont AUCUN label, la liste des
// noms est fixe et écrite en dur dans RenderPrometheus. La cardinalité est
// donc bornée par construction (une série par nom, cinq au total), sans
// qu'aucun garde-fou runtime ne soit nécessaire.
package metrics

import (
	"strconv"
	"strings"
	"sync/atomic"
)

// Counters porte l'état des compteurs. Le type est exporté pour que les
// tests puissent instancier un jeu de compteurs isolé ; le code applicatif
// utilise l'instance par défaut via les fonctions de paquet.
type Counters struct {
	updates       atomic.Int64
	updateErrors  atomic.Int64
	outboxRetries atomic.Int64
	deletions     atomic.Int64
	outboxBacklog atomic.Int64
}

// std est l'instance utilisée par le binaire. Les compteurs sont atomiques :
// le poller est séquentiel, mais l'outbox et la boucle de backlog tournent
// dans leurs propres goroutines et écrivent en concurrence.
var std = &Counters{}

// Default renvoie l'instance de paquet, à passer au serveur de santé.
func Default() *Counters { return std }

func (c *Counters) AddUpdates(n int64)       { c.updates.Add(n) }
func (c *Counters) AddUpdateErrors(n int64)  { c.updateErrors.Add(n) }
func (c *Counters) AddOutboxRetries(n int64) { c.outboxRetries.Add(n) }
func (c *Counters) AddDeletions(n int64)     { c.deletions.Add(n) }

// SetOutboxBacklog publie le nombre de lignes d'outbox restant à livrer.
// C'est une jauge : elle monte et descend, contrairement aux compteurs.
func (c *Counters) SetOutboxBacklog(n int64) { c.outboxBacklog.Store(n) }

// AddUpdates et consorts, version instance par défaut.
func AddUpdates(n int64)       { std.AddUpdates(n) }
func AddUpdateErrors(n int64)  { std.AddUpdateErrors(n) }
func AddOutboxRetries(n int64) { std.AddOutboxRetries(n) }
func AddDeletions(n int64)     { std.AddDeletions(n) }
func SetOutboxBacklog(n int64) { std.SetOutboxBacklog(n) }

// ContentType est le type MIME de l'exposition texte Prometheus.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// series décrit une série exposée. Le tableau est la liste EXHAUSTIVE des
// noms possibles : ajouter une série se fait ici, jamais dynamiquement à
// partir d'une donnée d'update.
type series struct {
	name  string
	help  string
	kind  string // "counter" ou "gauge"
	value func(*Counters) int64
}

var allSeries = []series{
	{
		name:  "undelete_updates_total",
		help:  "Nombre total d'updates Telegram reçus et remis au handler.",
		kind:  "counter",
		value: func(c *Counters) int64 { return c.updates.Load() },
	},
	{
		name:  "undelete_update_errors_total",
		help:  "Nombre total d'erreurs de récupération ou de traitement d'update.",
		kind:  "counter",
		value: func(c *Counters) int64 { return c.updateErrors.Load() },
	},
	{
		name:  "undelete_outbox_retries_total",
		help:  "Nombre total de replanifications d'alertes de l'outbox.",
		kind:  "counter",
		value: func(c *Counters) int64 { return c.outboxRetries.Load() },
	},
	{
		name:  "undelete_deletions_total",
		help:  "Nombre total de messages supprimés retrouvés et marqués en base.",
		kind:  "counter",
		value: func(c *Counters) int64 { return c.deletions.Load() },
	},
	{
		name:  "undelete_outbox_backlog",
		help:  "Nombre d'alertes de l'outbox restant à livrer (pending ou processing).",
		kind:  "gauge",
		value: func(c *Counters) int64 { return c.outboxBacklog.Load() },
	},
}

// RenderPrometheus rend l'exposition texte de l'instance par défaut.
func RenderPrometheus() string { return std.RenderPrometheus() }

// RenderPrometheus rend l'exposition texte : un bloc HELP/TYPE puis la valeur
// pour chaque série. Aucun label n'est émis, donc aucune valeur issue d'un
// update ne peut se retrouver dans la sortie.
func (c *Counters) RenderPrometheus() string {
	var b strings.Builder
	for _, s := range allSeries {
		b.WriteString("# HELP ")
		b.WriteString(s.name)
		b.WriteString(" ")
		b.WriteString(s.help)
		b.WriteString("\n# TYPE ")
		b.WriteString(s.name)
		b.WriteString(" ")
		b.WriteString(s.kind)
		b.WriteString("\n")
		b.WriteString(s.name)
		b.WriteString(" ")
		b.WriteString(strconv.FormatInt(s.value(c), 10))
		b.WriteString("\n")
	}
	return b.String()
}
