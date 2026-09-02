package telegram

import (
	"encoding/json"
	"testing"
)

func TestBusinessConnectionCanReplyFromRights(t *testing.T) {
	var connection BusinessConnection
	if err := json.Unmarshal([]byte(`{
		"id":"connection-1",
		"user":{"id":42,"is_bot":false,"first_name":"Louis"},
		"user_chat_id":42,
		"date":1,
		"is_enabled":true,
		"rights":{"can_reply":true}
	}`), &connection); err != nil {
		t.Fatalf("json.Unmarshal() error: %v", err)
	}
	if !connection.CanReply() {
		t.Fatal("CanReply() = false despite rights.can_reply=true")
	}
}

func TestBusinessConnectionCanReplyLegacy(t *testing.T) {
	connection := BusinessConnection{CanReplyLegacy: true}
	if !connection.CanReply() {
		t.Fatal("CanReply() = false for the legacy can_reply field")
	}
}
