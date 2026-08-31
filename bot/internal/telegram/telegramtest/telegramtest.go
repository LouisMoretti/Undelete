// Package telegramtest fournit les helpers partagés par les tests de contrat
// Bot API. Il vit à part du package telegram pour être importable par les
// packages appelants (app, business), qui doivent pouvoir exercer les chemins
// de production réels -- construction de l'alerte incluse -- contre les mêmes
// fixtures que le client.
//
// Convention de comparaison (une seule, valable pour toutes les fixtures) :
// les octets bruts du corps HTTP sont comparés à ceux du fichier via
// bytes.Equal. La SEULE normalisation appliquée est le retrait de l'unique
// "\n" terminal de stockage exigé par Fixture ; aucun espace, aucune
// indentation, aucun CRLF n'est toléré ni nettoyé ailleurs.
package telegramtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

// Version est le répertoire de fixtures figé pour la version de Bot API
// couverte (cf. testdata/README.md).
const Version = "bot-api-10.3"

// OKEnvelopeFixture est un stub de TRANSPORT, pas un contrat : il vérifie
// seulement que le client accepte une enveloppe {"ok": true}. Son `result`
// vide ne décrit aucun scénario et ne doit jamais être lu comme la réponse
// attendue pour un sendMessage donné (SendMessage ignore `result`).
const OKEnvelopeFixture = "send-message-ok-envelope.json"

// Token est le jeton factice utilisé dans le chemin d'URL vérifié.
const Token = "test-token"

// fixtureDir localise testdata via le fichier source de ce package plutôt que
// via le répertoire courant : les tests appelants (app, business) s'exécutent
// depuis leur propre répertoire de package.
func fixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata", Version)
}

// Fixture lit une fixture et renvoie ses octets, privés de l'unique newline LF
// terminale de stockage. Toute autre terminaison (absente, doublée, CRLF) est
// une erreur : c'est ce qui rend la comparaison octet par octet exacte plutôt
// qu'approximative (cf. commentaire de package).
func Fixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir(), name))
	if err != nil {
		t.Fatalf("lecture fixture %s: %v", name, err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) || bytes.HasSuffix(data, []byte("\n\n")) || bytes.HasSuffix(data, []byte("\r\n")) {
		t.Fatalf("fixture %s: attendu exactement une newline LF finale", name)
	}
	return data[:len(data)-1]
}

// Call décrit un appel HTTP attendu, dans l'ordre.
type Call struct {
	// Method est la méthode Bot API attendue (getUpdates, sendMessage, ...).
	Method string
	// RequestFixture est comparée octet par octet au corps envoyé.
	RequestFixture string
	// ResponseFixture est renvoyée telle quelle au client.
	ResponseFixture string
}

// NewClient renvoie un vrai telegram.Client pointé vers un serveur httptest
// qui vérifie chaque appel contre calls, dans l'ordre, et échoue au nettoyage
// si tous les appels attendus n'ont pas eu lieu.
func NewClient(t testing.TB, calls ...Call) *telegram.Client {
	t.Helper()

	var (
		mu sync.Mutex
		n  int
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		index := n
		n++
		mu.Unlock()

		if index >= len(calls) {
			t.Errorf("appel HTTP n°%d inattendu (%s), %d attendus", index+1, r.URL.Path, len(calls))
			// 4xx : définitif côté client, donc pas de retry ni d'attente.
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":false,"error_code":400,"description":"appel inattendu"}`))
			return
		}
		call := calls[index]

		if r.Method != http.MethodPost {
			t.Errorf("appel %d: méthode HTTP = %s, attendu POST", index+1, r.Method)
		}
		if want := "/bot" + Token + "/" + call.Method; r.URL.Path != want {
			t.Errorf("appel %d: chemin = %s, attendu %s", index+1, r.URL.Path, want)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("appel %d: Content-Type = %q, attendu application/json", index+1, got)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("appel %d: lecture requête: %v", index+1, err)
			return
		}
		if want := Fixture(t, call.RequestFixture); !bytes.Equal(body, want) {
			t.Errorf("appel %d: JSON %s = %s, attendu %s", index+1, call.Method, body, want)
		}

		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(Fixture(t, call.ResponseFixture)); err != nil {
			t.Errorf("appel %d: écriture réponse: %v", index+1, err)
		}
	}))

	t.Cleanup(func() {
		server.Close()
		mu.Lock()
		defer mu.Unlock()
		if n != len(calls) {
			t.Errorf("nombre d'appels HTTP = %d, attendu %d", n, len(calls))
		}
	})

	return telegram.NewClient(Token, time.Second, telegram.WithBaseURL(server.URL+"/bot"))
}

// NewRecordingClient renvoie un vrai telegram.Client qui répond toujours
// l'enveloppe de succès et enregistre les corps de requête émis, dans l'ordre.
//
// À utiliser quand ce qui compte est le NOMBRE et la SUITE d'appels produits
// par un chemin de production (une notification découpée en plusieurs
// sendMessage, par exemple) plutôt que la conformité à une fixture figée.
func NewRecordingClient(t testing.TB) (*telegram.Client, func() [][]byte) {
	t.Helper()

	var (
		mu     sync.Mutex
		bodies [][]byte
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("lecture requête enregistrée: %v", err)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(Fixture(t, OKEnvelopeFixture)); err != nil {
			t.Errorf("écriture réponse: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client := telegram.NewClient(Token, time.Second, telegram.WithBaseURL(server.URL+"/bot"))
	return client, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies...)
	}
}

// AssertNoBusinessConnectionID échoue si la charge utile sérialisée expose,
// à n'importe quelle profondeur, une CLÉ business_connection_id.
//
// Contrainte n°7, vérifiée sur le JSON réellement émis : aucune référence à
// un nom de champ Go, donc le garde-fou survit à toute évolution du type
// SendMessageRequest (renommage comme suppression de champ). La vérification
// porte sur les clés et non sur une sous-chaîne : un message supprimé dont le
// texte contiendrait ces mots reste une notification légitime.
func AssertNoBusinessConnectionID(t testing.TB, payload []byte) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("charge utile sendMessage illisible: %v", err)
	}
	if path := findKey(decoded, "business_connection_id", ""); path != "" {
		t.Fatalf("charge utile sendMessage exposant business_connection_id en %s: %s", path, payload)
	}
}

// findKey renvoie le chemin de la première occurrence de key, ou "".
func findKey(value any, key, path string) string {
	switch v := value.(type) {
	case map[string]any:
		for name, child := range v {
			childPath := path + "." + name
			if name == key {
				return childPath
			}
			if found := findKey(child, key, childPath); found != "" {
				return found
			}
		}
	case []any:
		for i, child := range v {
			if found := findKey(child, key, fmt.Sprintf("%s[%d]", path, i)); found != "" {
				return found
			}
		}
	}
	return ""
}
