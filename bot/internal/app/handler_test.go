package app

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func TestSplitTelegramTextPreservesContentAndUTF16Limit(t *testing.T) {
	original := strings.Repeat("a", 4095) + "😀" + strings.Repeat("é", 10)
	chunks := splitTelegramText(original, telegramTextLimit)

	if len(chunks) != 2 {
		t.Fatalf("nombre de chunks = %d, attendu 2", len(chunks))
	}
	if strings.Join(chunks, "") != original {
		t.Fatal("le découpage a modifié ou tronqué le contenu")
	}
	for i, chunk := range chunks {
		units := 0
		for _, r := range chunk {
			units += utf16.RuneLen(r)
		}
		if units > telegramTextLimit {
			t.Fatalf("chunk %d contient %d unités UTF-16", i, units)
		}
	}
}

func TestSplitTelegramTextRejectsInvalidInput(t *testing.T) {
	if chunks := splitTelegramText("", telegramTextLimit); chunks != nil {
		t.Fatalf("texte vide: chunks = %#v, attendu nil", chunks)
	}
	if chunks := splitTelegramText("test", 0); chunks != nil {
		t.Fatalf("limite invalide: chunks = %#v, attendu nil", chunks)
	}
}
