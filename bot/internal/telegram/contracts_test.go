// Ce fichier est en package telegram_test (test externe) et non en package
// telegram : il importe telegramtest, qui importe lui-même telegram. Le test
// externe est ce qui rend cette dépendance légale, et il garantit au passage
// que les contrats sont vérifiés via l'API publique du package, comme le font
// app et business.
package telegram_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram/telegramtest"
)

func TestGetUpdatesBotAPIContract(t *testing.T) {
	client := telegramtest.NewClient(t, telegramtest.Call{
		Method:          "getUpdates",
		RequestFixture:  "get-updates-request.json",
		ResponseFixture: "get-updates-response.json",
	})
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
	client := telegramtest.NewClient(t, telegramtest.Call{
		Method:          "getBusinessConnection",
		RequestFixture:  "get-business-connection-request.json",
		ResponseFixture: "get-business-connection-response.json",
	})
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

// TestGetBusinessConnectionLegacyCanReplyContract fige au filaire le chemin
// legacy de BusinessConnection : `can_reply` posé directement sur la connexion,
// sans bloc `rights`. Ce chemin n'était jusqu'ici exercé qu'en process (à
// partir d'une structure Go construite à la main) ; la fixture prouve qu'une
// réponse Bot API réellement formée ainsi est décodée comme « peut répondre »
// et non silencieusement comme false.
func TestGetBusinessConnectionLegacyCanReplyContract(t *testing.T) {
	client := telegramtest.NewClient(t, telegramtest.Call{
		Method:          "getBusinessConnection",
		RequestFixture:  "get-business-connection-request.json",
		ResponseFixture: "get-business-connection-legacy-can-reply-response.json",
	})
	connection, err := client.GetBusinessConnection(context.Background(), "bc_fixture_001")
	if err != nil {
		t.Fatalf("GetBusinessConnection(): %v", err)
	}
	if connection.Rights != nil {
		t.Fatalf("la fixture legacy ne doit porter aucun bloc rights: %#v", connection.Rights)
	}
	if !connection.CanReplyLegacy || !connection.CanReply() {
		t.Fatalf("can_reply legacy mal décodé: %#v", connection)
	}
}

// TestGetUpdatesFirstPollOffsetZeroContract fige la sérialisation du TOUT
// premier poll : `offset` est `omitempty`, donc l'offset 0 n'apparaît pas dans
// le corps émis. C'est le comportement attendu — Telegram traite l'absence
// d'offset comme « depuis le plus ancien update non confirmé » — mais il n'est
// figé nulle part ailleurs, alors qu'il conditionne le premier appel de chaque
// démarrage du bot.
func TestGetUpdatesFirstPollOffsetZeroContract(t *testing.T) {
	client := telegramtest.NewClient(t, telegramtest.Call{
		Method:          "getUpdates",
		RequestFixture:  "get-updates-first-poll-request.json",
		ResponseFixture: "get-updates-empty-response.json",
	})
	updates, err := client.GetUpdates(context.Background(), 0, 50)
	if err != nil {
		t.Fatalf("GetUpdates(): %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("nombre d'updates = %d, attendu 0", len(updates))
	}

	// Le corps a déjà été comparé octet par octet par le serveur de test ; on
	// vérifie ici que c'est bien l'ABSENCE de la clé qui est figée, et non un
	// "offset":0 qui aurait été recopié dans la fixture.
	var body map[string]any
	if err := json.Unmarshal(telegramtest.Fixture(t, "get-updates-first-poll-request.json"), &body); err != nil {
		t.Fatalf("décodage fixture premier poll: %v", err)
	}
	if _, ok := body["offset"]; ok {
		t.Fatalf("la fixture du premier poll sérialise offset: %v", body["offset"])
	}
}

// TestSendMessageRetryAfterContract fige la requête RÉÉMISE après une
// enveloppe 429 : le client doit respecter retry_after puis repartir avec des
// octets strictement identiques. L'enveloppe elle-même n'était couverte qu'en
// process ; le mécanisme octet par octet vérifie en plus qu'aucun paramètre
// n'est ajouté ni perdu à la seconde tentative.
//
// retry_after vaut 1 seconde dans la fixture : suffisant pour exercer
// l'attente réelle de SendMessage sans allonger la suite de tests.
func TestSendMessageRetryAfterContract(t *testing.T) {
	const fixture = "send-message-welcome-request.json"
	client := telegramtest.NewClient(t,
		telegramtest.Call{
			Method:          "sendMessage",
			RequestFixture:  fixture,
			ResponseFixture: "send-message-rate-limited-response.json",
		},
		telegramtest.Call{
			Method:          "sendMessage",
			RequestFixture:  fixture,
			ResponseFixture: telegramtest.OKEnvelopeFixture,
		},
	)

	start := time.Now()
	if err := client.SendMessage(context.Background(), telegram.BuildWelcomeMessageRequest(700002, 700001)); err != nil {
		t.Fatalf("SendMessage(): %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("retry_after non respecté: réémission après %s, attendu >= 1s", elapsed)
	}
}

// TestRateLimitedEnvelopeIsDecodedAsAPIError fige la lecture de l'enveloppe 429
// elle-même : code et retry_after doivent remonter dans *telegram.APIError,
// seule voie par laquelle le poller et SendMessage savent combien attendre.
func TestRateLimitedEnvelopeIsDecodedAsAPIError(t *testing.T) {
	client := telegramtest.NewClient(t, telegramtest.Call{
		Method:          "sendMessage",
		RequestFixture:  "send-message-welcome-request.json",
		ResponseFixture: "send-message-rate-limited-response.json",
	})

	err := client.SendMessageOnce(context.Background(), telegram.BuildWelcomeMessageRequest(700002, 700001))
	var apiErr *telegram.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("SendMessageOnce() = %v, attendu *telegram.APIError", err)
	}
	if apiErr.Code != http.StatusTooManyRequests || apiErr.RetryAfter != 1 || !apiErr.IsRateLimited() {
		t.Fatalf("enveloppe 429 mal décodée: %#v", apiErr)
	}
}

// TestSendMessageRequestNeverSerializesBusinessConnectionID matérialise la
// contrainte n°7 sans jamais nommer un champ Go : le type est inspecté par ses
// tags JSON (récursivement, champs embarqués inclus) puis par la charge utile
// qu'il produit réellement. Le test reste donc valide si SendMessageRequest
// gagne, perd ou renomme des champs.
func TestSendMessageRequestNeverSerializesBusinessConnectionID(t *testing.T) {
	assertNoBusinessConnectionIDTag(t, reflect.TypeOf(telegram.SendMessageRequest{}), "SendMessageRequest")

	payload, err := json.Marshal(telegram.SendMessageRequest{ChatID: 700001, Text: "charge utile de contrôle"})
	if err != nil {
		t.Fatalf("sérialisation SendMessageRequest: %v", err)
	}
	telegramtest.AssertNoBusinessConnectionID(t, payload)
}

// fixtureSenderID matérialise un from_user_id renseigné (la colonne est
// NULLable, d'où le pointeur).
func fixtureSenderID() *int64 {
	id := int64(800001)
	return &id
}

func assertNoBusinessConnectionIDTag(t *testing.T, typ reflect.Type, path string) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue // non sérialisé par encoding/json
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		if name == "business_connection_id" {
			t.Fatalf("%s.%s sérialise business_connection_id", path, field.Name)
		}
		// Un champ embarqué remonte ses propres clés au niveau parent : ses
		// tags comptent donc autant que ceux du type lui-même.
		if field.Anonymous {
			assertNoBusinessConnectionIDTag(t, field.Type, path+"."+field.Name)
		}
	}
}

// TestSendMessageAlertContracts fige les deux charges utiles d'alerte telles
// que construites par les builders de production, octet par octet.
//
// Les chemins d'appel qui alimentent ces builders en production sont couverts
// à leur propre niveau, faute de quoi ce test ne prouverait rien sur ce que le
// bot envoie vraiment : business.TestWelcomeAlertContract (bienvenue) et, pour
// la suppression, messages.Repository.MarkDeleted -- qui écrit ces mêmes
// chunks en outbox -- vérifié par la suite d'intégration PostgreSQL
// (« chat labels are tenant isolated and reach the alert »).
func TestSendMessageAlertContracts(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		requests []telegram.SendMessageRequest
	}{
		{
			name:     "welcome",
			fixture:  "send-message-welcome-request.json",
			requests: []telegram.SendMessageRequest{telegram.BuildWelcomeMessageRequest(700002, 700001)},
		},
		{
			name:    "deletion",
			fixture: "send-message-deletion-request.json",
			// Même scénario que l'update business_message de
			// get-updates-response.json, vu depuis la suppression : chat privé
			// « Anaïs » (800001), expéditeur homonyme, date d'envoi du message.
			requests: telegram.BuildDeletionMessageRequests(telegram.DeletionAlert{
				OwnerTelegramUserID: 700001,
				ChatID:              800001,
				ChatTitle:           "Anaïs",
				FromDisplay:         "Anaïs (@fixture_sender)",
				FromUserID:          fixtureSenderID(),
				MessageType:         "text",
				TelegramDate:        1788019201,
				Content:             "Bonjour, café ☕ — déjà vu ?",
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.requests) != 1 {
				t.Fatalf("nombre de requêtes = %d, attendu 1 pour cette fixture", len(tt.requests))
			}
			client := telegramtest.NewClient(t, telegramtest.Call{
				Method:          "sendMessage",
				RequestFixture:  tt.fixture,
				ResponseFixture: telegramtest.OKEnvelopeFixture,
			})
			if err := client.SendMessage(context.Background(), tt.requests[0]); err != nil {
				t.Fatalf("SendMessage(): %v", err)
			}
			telegramtest.AssertNoBusinessConnectionID(t, telegramtest.Fixture(t, tt.fixture))
		})
	}
}

// TestWelcomeMessageTextIsTheProductionText verrouille le texte de bienvenue
// figé dans la fixture sur celui que produit le builder de production : la
// fixture ne peut pas dériver vers un texte « de test ».
func TestWelcomeMessageTextIsTheProductionText(t *testing.T) {
	var fixture struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(telegramtest.Fixture(t, "send-message-welcome-request.json"), &fixture); err != nil {
		t.Fatalf("décodage fixture bienvenue: %v", err)
	}
	if got := telegram.BuildWelcomeMessageRequest(700002, 700001).Text; got != fixture.Text {
		t.Fatalf("texte de bienvenue de production = %q, fixture = %q", got, fixture.Text)
	}
}
