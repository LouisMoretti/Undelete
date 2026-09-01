package metrics

import (
	"strings"
	"testing"
)

// expectedSeries fige le contrat d'exposition : cette liste EST la
// cardinalité totale de /metrics. Un ajout de série doit être un choix
// délibéré, visible en revue, pas un effet de bord.
var expectedSeries = []string{
	"undelete_updates_total",
	"undelete_update_errors_total",
	"undelete_outbox_retries_total",
	"undelete_outbox_failed_total",
	"undelete_deletions_total",
	"undelete_outbox_backlog",
}

func TestRenderPrometheusExposeLesSeriesAttendues(t *testing.T) {
	c := &Counters{}
	c.AddUpdates(3)
	c.AddUpdateErrors(1)
	c.AddOutboxRetries(2)
	c.AddOutboxFailed(6)
	c.AddDeletions(7)
	c.SetOutboxBacklog(4)

	out := c.RenderPrometheus()

	for _, want := range []string{
		"undelete_updates_total 3",
		"undelete_update_errors_total 1",
		"undelete_outbox_retries_total 2",
		"undelete_outbox_failed_total 6",
		"undelete_deletions_total 7",
		"undelete_outbox_backlog 4",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("sortie sans %q:\n%s", want, out)
		}
	}

	for _, name := range expectedSeries {
		if !strings.Contains(out, "# HELP "+name+" ") || !strings.Contains(out, "# TYPE "+name+" ") {
			t.Fatalf("série %q sans HELP/TYPE:\n%s", name, out)
		}
	}
}

// TestRenderPrometheusNEmetAucunLabel est le garde-fou de confidentialité :
// aucun label n'est émis, donc aucun id, nom, texte ou jeton ne peut se
// glisser dans l'exposition. Un `{` sur une ligne de valeur signalerait
// l'apparition d'une dimension dynamique.
func TestRenderPrometheusNEmetAucunLabel(t *testing.T) {
	c := &Counters{}
	c.AddUpdates(1)

	out := c.RenderPrometheus()
	names := map[string]bool{}

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.ContainsAny(line, "{}") {
			t.Fatalf("label détecté dans la ligne %q", line)
		}
		name, _, found := strings.Cut(line, " ")
		if !found {
			t.Fatalf("ligne de métrique malformée: %q", line)
		}
		if names[name] {
			t.Fatalf("série %q exposée deux fois", name)
		}
		names[name] = true
	}

	if len(names) != len(expectedSeries) {
		t.Fatalf("nombre de séries = %d, attendu %d (cardinalité bornée)", len(names), len(expectedSeries))
	}
	for _, want := range expectedSeries {
		if !names[want] {
			t.Fatalf("série %q absente de l'exposition", want)
		}
	}
}

func TestCountersSontIndependantsDeLInstanceParDefaut(t *testing.T) {
	before := Default().RenderPrometheus()

	c := &Counters{}
	c.AddDeletions(42)

	if Default().RenderPrometheus() != before {
		t.Fatal("un jeu de compteurs isolé a modifié l'instance par défaut")
	}
}
