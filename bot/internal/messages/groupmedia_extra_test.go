package messages

import (
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
)

// TestGroupMediaAlbumWithoutAnchorsFallsBackToFirstSeen pins the degraded
// catalogue path: when the anchors read returned nothing for a group, the
// anchor is the first message seen (previous behaviour), and the album still
// holds together.
func TestGroupMediaAlbumWithoutAnchorsFallsBackToFirstSeen(t *testing.T) {
	files := []media.File{
		storedFile(1, 100, 0, "G"),
		storedFile(2, 101, 0, "G"),
	}
	groups := groupMedia(files, map[string]int64{})
	if len(groups) != 1 || groups[0].anchorMessageID != 100 {
		t.Fatalf("anchor must fall back to the first message seen: %+v", groups)
	}
	if len(groups[0].files) != 2 {
		t.Fatalf("album must hold together without anchors: %+v", groups)
	}
}

// TestGroupMediaIgnoresOrphanAnchors pins that anchors for groups absent from
// the input change nothing: no empty group is created from an orphan key.
func TestGroupMediaIgnoresOrphanAnchors(t *testing.T) {
	files := []media.File{storedFile(1, 100, 0, "G")}
	groups := groupMedia(files, map[string]int64{"G": 100, "GHOST": 999})
	if len(groups) != 1 {
		t.Fatalf("orphan anchors must not create groups, got %d", len(groups))
	}
}

// TestGroupMediaEmptyInputProducesNoGroup pins the no-media deletion path:
// a text deletion carries no files and must produce no media group.
func TestGroupMediaEmptyInputProducesNoGroup(t *testing.T) {
	if groups := groupMedia(nil, map[string]int64{}); len(groups) != 0 {
		t.Fatalf("nil input must produce no group, got %+v", groups)
	}
}

// TestGroupMediaInterleavedAlbumsStaySeparate pins multi-album deletions: two
// albums interleaved in one deletion batch keep their own groups and anchors.
func TestGroupMediaInterleavedAlbumsStaySeparate(t *testing.T) {
	files := []media.File{
		storedFile(1, 100, 0, "G"),
		storedFile(2, 200, 0, "H"),
		storedFile(3, 101, 0, "G"),
		storedFile(4, 201, 0, "H"),
	}
	groups := groupMedia(files, map[string]int64{"G": 100, "H": 200})
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 albums", len(groups))
	}
	if groups[0].mediaGroupID != "G" || len(groups[0].files) != 2 {
		t.Fatalf("album G = %+v", groups[0])
	}
	if groups[1].mediaGroupID != "H" || len(groups[1].files) != 2 || groups[1].anchorMessageID != 200 {
		t.Fatalf("album H = %+v", groups[1])
	}
}
