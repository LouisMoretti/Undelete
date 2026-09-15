package messages

import (
	"context"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
)

// TestNewRepositoryReturnsUsableRepository pins the constructor: it wraps the
// given handle without touching the database, so it must not require a live
// pool to return a usable repository.
func TestNewRepositoryReturnsUsableRepository(t *testing.T) {
	if got := NewRepository(nil); got == nil {
		t.Fatal("NewRepository(nil) = nil, want a usable repository")
	}
}

// TestEnqueueMediaAlertsNoopOnEmptyBatch pins the text-only deletion path: a
// deletion batch with no message found carries no files and must enqueue
// nothing, without touching the transaction. The nil tx is the point: any
// query attempt would panic, so passing proves no query happens.
func TestEnqueueMediaAlertsNoopOnEmptyBatch(t *testing.T) {
	scope := mediaAlertScope{
		ownerUserID:          11,
		ownerTelegramUserID:  22,
		businessConnectionID: "bc-synthetic-001",
		chatID:               33,
	}
	for _, tc := range []struct {
		name  string
		found []DeletedRecord
	}{
		{name: "nil batch", found: nil},
		{name: "empty batch", found: []DeletedRecord{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunkCount := map[int64]int{}
			if err := enqueueMediaAlerts(context.Background(), nil, scope, tc.found, chunkCount); err != nil {
				t.Fatalf("enqueueMediaAlerts(empty) = %v, want nil", err)
			}
			if len(chunkCount) != 0 {
				t.Fatalf("chunkCount = %v after an empty batch, want untouched", chunkCount)
			}
		})
	}
}

// TestGroupMediaSameMessageAlbumAndLoneStaySeparate pins the two key spaces:
// a media without media_group_id is only ever grouped with the other files
// of its OWN message, even when it shares that message with an album file.
// A single map keyed by message would merge them.
func TestGroupMediaSameMessageAlbumAndLoneStaySeparate(t *testing.T) {
	files := []media.File{
		storedFile(1, 100, 0, "G"),
		storedFile(2, 100, 0, ""),
		storedFile(3, 100, 1, ""),
	}
	groups := groupMedia(files, map[string]int64{"G": 100})
	if len(groups) != 2 {
		t.Fatalf("%d groups, want 2 (album G and lone message 100)", len(groups))
	}
	if groups[0].mediaGroupID != "G" || len(groups[0].files) != 1 {
		t.Fatalf("album G = %#v, want its single file alone", groups[0])
	}
	if groups[1].mediaGroupID != "" || len(groups[1].files) != 2 {
		t.Fatalf("lone message 100 = %#v, want its two files together", groups[1])
	}
	if groups[1].anchorMessageID != 100 {
		t.Fatalf("lone anchor = %d, want 100", groups[1].anchorMessageID)
	}
}

// TestGroupMediaSameMessageAlbumFilesStayTogether pins that several files of
// one album carried by the SAME message still produce one entry: the album
// key is the media_group_id, never (message, group).
func TestGroupMediaSameMessageAlbumFilesStayTogether(t *testing.T) {
	files := []media.File{
		storedFile(1, 100, 0, "G"),
		storedFile(2, 100, 1, "G"),
	}
	groups := groupMedia(files, map[string]int64{"G": 100})
	if len(groups) != 1 {
		t.Fatalf("%d groups, want 1 album", len(groups))
	}
	if len(groups[0].files) != 2 {
		t.Fatalf("%d files in the album, want 2", len(groups[0].files))
	}
	if groups[0].anchorMessageID != 100 {
		t.Fatalf("anchor = %d, want the catalogued anchor 100", groups[0].anchorMessageID)
	}
}

// TestGroupMediaLoneMediaIgnoresAnchors pins that a lone media never consults
// the anchors map: its anchor is its own message, even when the map carries
// an entry keyed by the empty group id. An unconditional lookup of
// anchors[file.MediaGroupID] would return 999 here.
func TestGroupMediaLoneMediaIgnoresAnchors(t *testing.T) {
	files := []media.File{storedFile(1, 101, 0, "")}
	groups := groupMedia(files, map[string]int64{"": 999, "G": 100})
	if len(groups) != 1 {
		t.Fatalf("%d groups, want 1 lone media", len(groups))
	}
	if groups[0].anchorMessageID != 101 {
		t.Fatalf("lone anchor = %d, want its own message 101", groups[0].anchorMessageID)
	}
	if groups[0].mediaGroupID != "" {
		t.Fatalf("lone mediaGroupID = %q, want empty", groups[0].mediaGroupID)
	}
}

// TestGroupMediaMixedAnchorsDegradesPerGroup pins partial catalogue reads: an
// album the anchors map knows keeps its stable anchor while an album it
// misses falls back to the first message seen. Degradation is per group,
// never all-or-nothing.
func TestGroupMediaMixedAnchorsDegradesPerGroup(t *testing.T) {
	files := []media.File{
		storedFile(1, 100, 0, "G"),
		storedFile(2, 101, 0, "G"),
		storedFile(3, 200, 0, "H"),
	}
	groups := groupMedia(files, map[string]int64{"G": 100})
	if len(groups) != 2 {
		t.Fatalf("%d groups, want 2 albums", len(groups))
	}
	if groups[0].mediaGroupID != "G" || groups[0].anchorMessageID != 100 {
		t.Fatalf("album G = %#v, want the catalogued anchor 100", groups[0])
	}
	if groups[1].mediaGroupID != "H" || groups[1].anchorMessageID != 200 {
		t.Fatalf("album H = %#v, want a fallback to the first message 200", groups[1])
	}
}

// TestGroupMediaPreservesFirstSeenOrder pins that the output order follows the
// input order, not the anchor or message order: a lone media seen first stays
// first even when its message id is the largest.
func TestGroupMediaPreservesFirstSeenOrder(t *testing.T) {
	files := []media.File{
		storedFile(9, 300, 0, ""),
		storedFile(1, 100, 0, "G"),
		storedFile(2, 101, 0, "G"),
	}
	groups := groupMedia(files, map[string]int64{"G": 100})
	if len(groups) != 2 {
		t.Fatalf("%d groups, want 2", len(groups))
	}
	if groups[0].mediaGroupID != "" || groups[0].anchorMessageID != 300 {
		t.Fatalf("first group = %#v, want the lone message 300 seen first", groups[0])
	}
	if groups[1].mediaGroupID != "G" {
		t.Fatalf("second group = %#v, want album G", groups[1])
	}
}

// TestGroupMediaPreservesInputOrderWithinGroup pins the no-sort contract: the
// files of one entry keep the order SelectStoredTx gave (message_id, then
// file_index). groupMedia must not reorder, even when the input arrives in a
// surprising file_index order.
func TestGroupMediaPreservesInputOrderWithinGroup(t *testing.T) {
	first := storedFile(2, 101, 1, "G")
	second := storedFile(1, 100, 0, "G")
	groups := groupMedia([]media.File{first, second}, map[string]int64{"G": 100})
	if len(groups) != 1 || len(groups[0].files) != 2 {
		t.Fatalf("groups = %#v, want one album of two files", groups)
	}
	if groups[0].files[0].ID != first.ID || groups[0].files[1].ID != second.ID {
		t.Fatalf("within-group order = [%d %d], want input order [%d %d]",
			groups[0].files[0].ID, groups[0].files[1].ID, first.ID, second.ID)
	}
}

// TestGroupMediaEmptyFilesWithAnchorsYieldsNoGroup pins that anchors never
// create groups on their own: a text deletion (no files) produces no media
// entry whatever the catalogue read returned.
func TestGroupMediaEmptyFilesWithAnchorsYieldsNoGroup(t *testing.T) {
	if groups := groupMedia(nil, map[string]int64{"G": 100, "H": 200}); len(groups) != 0 {
		t.Fatalf("groupMedia(nil, anchors) = %#v, want no group", groups)
	}
}

// TestGroupMediaLoneMessageThreeFilesStayTogether pins the N-file lone path:
// every file of one message without media_group_id travels in the same entry,
// in input order.
func TestGroupMediaLoneMessageThreeFilesStayTogether(t *testing.T) {
	files := []media.File{
		storedFile(1, 101, 0, ""),
		storedFile(2, 101, 1, ""),
		storedFile(3, 101, 2, ""),
	}
	groups := groupMedia(files, map[string]int64{"G": 100})
	if len(groups) != 1 {
		t.Fatalf("%d groups, want 1 lone message", len(groups))
	}
	if len(groups[0].files) != 3 {
		t.Fatalf("%d files, want 3", len(groups[0].files))
	}
	for i, want := range []int{0, 1, 2} {
		if groups[0].files[i].FileIndex != want {
			t.Fatalf("file %d has index %d, want %d (input order)", i, groups[0].files[i].FileIndex, want)
		}
	}
}
