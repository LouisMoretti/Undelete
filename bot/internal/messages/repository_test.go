package messages

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func TestSplitUTF16PreservesContentAndTelegramLimit(t *testing.T) {
	original := strings.Repeat("a", 4095) + "😀" + strings.Repeat("é", 10)
	chunks := splitUTF16(original, 4096)
	if len(chunks) != 2 || strings.Join(chunks, "") != original {
		t.Fatalf("découpage invalide: %d chunks", len(chunks))
	}
	for i, chunk := range chunks {
		units := 0
		for _, r := range chunk {
			units += utf16.RuneLen(r)
		}
		if units > 4096 {
			t.Fatalf("chunk %d contient %d unités UTF-16", i, units)
		}
	}
}
