package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const fixtureVersion = "bot-api-10.3"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + fixtureVersion + "/" + name)
	if err != nil {
		t.Fatalf("lecture fixture %s: %v", name, err)
	}
	return []byte(strings.TrimSpace(string(data)))
}

func fixtureClient(t *testing.T, method, requestFixture, responseFixture string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("méthode HTTP = %s, attendu POST", r.Method)
		}
		if r.URL.Path != "/bottest-token/"+method {
			t.Errorf("chemin = %s, attendu /bottest-token/%s", r.URL.Path, method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, attendu application/json", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("lecture requête: %v", err)
		}
		want := fixture(t, requestFixture)
		if string(body) != string(want) {
			t.Errorf("JSON %s = %s, attendu %s", method, body, want)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(fixture(t, responseFixture)); err != nil {
			t.Fatalf("écriture réponse: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client := NewClient("test-token", time.Second)
	client.baseURL = server.URL + "/bot"
	return client
}

func TestGetUpdatesBotAPIContract(t *testing.T) {
	client := fixtureClient(t, "getUpdates", "get-updates-request.json", "get-updates-response.json")
	updates, err := client.GetUpdates(context.Background(), 9000, 50)
	if err != nil {
		t.Fatalf("GetUpdates(): %v", err)
	}
	if len(updates) != 4 {
		t.Fatalf("nombre d'updates = %d, attendu 4", len(updates))
	}

	connection := updates[0].BusinessConnection
	if connection == nil || connection.UserChatID != 700002 || !connection.CanReply() {
		t.Fatalf("business_connection mal décodée: %#v", connection)
	}
	message := updates[1].BusinessMessage
	if message == nil || message.From == nil || message.Text != "Bonjour, café ☕ — déjà vu ?" {
		t.Fatalf("business_message mal décodé: %#v", message)
	}
	edited := updates[2].EditedBusinessMessage
	if edited == nil || edited.From != nil || edited.Text != "Texte corrigé 🧪" {
		t.Fatalf("edited_business_message sans from mal décodé: %#v", edited)
	}
	deleted := updates[3].DeletedBusinessMessages
	if deleted == nil || !reflect.DeepEqual(deleted.MessageIDs, []int64{501, 502, 503}) {
		t.Fatalf("suppression groupée mal décodée: %#v", deleted)
	}
}

func TestGetBusinessConnectionBotAPIContract(t *testing.T) {
	client := fixtureClient(t, "getBusinessConnection", "get-business-connection-request.json", "get-business-connection-response.json")
	connection, err := client.GetBusinessConnection(context.Background(), "bc_fixture_001")
	if err != nil {
		t.Fatalf("GetBusinessConnection(): %v", err)
	}
	if connection.ID != "bc_fixture_001" || connection.UserChatID != 700002 || !connection.CanReply() {
		t.Fatalf("connexion mal décodée: %#v", connection)
	}
	if connection.User.LastName != "" || connection.User.Username != "" {
		t.Fatalf("champs utilisateur optionnels inattendus: %#v", connection.User)
	}
}

func TestSendMessageAlertContracts(t *testing.T) {
	if _, exists := reflect.TypeOf(SendMessageRequest{}).FieldByName("BusinessConnectionID"); exists {
		t.Fatal("SendMessageRequest permet encore business_connection_id pour une alerte")
	}

	tests := []struct {
		name    string
		fixture string
		request SendMessageRequest
	}{
		{
			name:    "welcome",
			fixture: "send-message-welcome-request.json",
			request: SendMessageRequest{ChatID: 700002, Text: "undelete est connecté. Alerte synthétique 🛡️"},
		},
		{
			name:    "deletion",
			fixture: "send-message-deletion-request.json",
			request: SendMessageRequest{ChatID: 700001, Text: "Message supprimé récupéré (chat 800001) :\n\nBonjour, café ☕ — déjà vu ?"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fixtureClient(t, "sendMessage", tt.fixture, "send-message-response.json")
			if err := client.SendMessage(context.Background(), tt.request); err != nil {
				t.Fatalf("SendMessage(): %v", err)
			}
			if strings.Contains(string(fixture(t, tt.fixture)), "business_connection_id") {
				t.Fatalf("fixture d'alerte %s contient business_connection_id", tt.fixture)
			}
		})
	}
}
