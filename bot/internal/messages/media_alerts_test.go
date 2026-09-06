package messages

import (
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
)

func storedFile(id, messageID int64, index int, groupID string) media.File {
	return media.File{
		ID:           id,
		MessageID:    messageID,
		FileIndex:    index,
		MediaType:    telegram.MediaTypePhoto,
		RelativePath: "11/a/x.jpg",
		MediaGroupID: groupID,
		Status:       "stored",
	}
}

// An album must produce ONE entry carrying all of its files, in the order
// SelectStoredTx gave (message_id, file_index): that is the order the sender
// composed it in, and the only one the owner can check against what vanished.
func TestGroupMediaKeepsAnAlbumTogetherAndInOrder(t *testing.T) {
	files := []media.File{
		storedFile(1, 100, 0, "G"),
		storedFile(2, 101, 0, "G"),
		storedFile(3, 102, 0, "G"),
	}
	groups := groupMedia(files, map[string]int64{"G": 100})

	if len(groups) != 1 {
		t.Fatalf("%d groups, want 1 album", len(groups))
	}
	if len(groups[0].files) != 3 {
		t.Fatalf("%d files in the album, want 3", len(groups[0].files))
	}
	for i, want := range []int64{100, 101, 102} {
		if groups[0].files[i].MessageID != want {
			t.Fatalf("file %d is message %d, want %d", i, groups[0].files[i].MessageID, want)
		}
	}
}

// Two key spaces that must not collide: a media without media_group_id is only
// ever grouped with the other files of its OWN message.
func TestGroupMediaSeparatesLoneMediaFromAlbums(t *testing.T) {
	files := []media.File{
		storedFile(1, 100, 0, "G"),
		storedFile(2, 101, 0, ""),
		storedFile(3, 101, 1, ""),
		storedFile(4, 102, 0, "G"),
		storedFile(5, 103, 0, "H"),
	}
	groups := groupMedia(files, map[string]int64{"G": 100, "H": 103})

	if len(groups) != 3 {
		t.Fatalf("%d groups, want 3 (album G, lone message 101, album H)", len(groups))
	}
	if len(groups[0].files) != 2 || groups[0].mediaGroupID != "G" {
		t.Fatalf("album G = %#v", groups[0])
	}
	if len(groups[1].files) != 2 || groups[1].anchorMessageID != 101 {
		t.Fatalf("lone message 101 = %#v", groups[1])
	}
	if len(groups[2].files) != 1 || groups[2].mediaGroupID != "H" {
		t.Fatalf("album H = %#v", groups[2])
	}
}

// Regression: the anchor keys the outbox anti-duplicate constraint
// (message_id, event_type, chunk_index). It must NOT depend on which downloads
// happened to be finished at delivery time -- otherwise a redelivered
// deleted_business_messages update, arriving after a pending file became
// stored, lands on a different message_id, misses the ON CONFLICT DO NOTHING,
// and the owner receives the album twice.
func TestGroupMediaAnchorIsStableWhenTheStoredSetGrows(t *testing.T) {
	// The catalogue knows the whole album from capture time, whatever the
	// status of each file.
	anchors := map[string]int64{"G": 100}

	// First delivery: message 100 is still downloading, only 101 and 102 are on
	// disk.
	partial := groupMedia([]media.File{
		storedFile(2, 101, 0, "G"),
		storedFile(3, 102, 0, "G"),
	}, anchors)

	// Redelivery: the download of 100 has since completed.
	complete := groupMedia([]media.File{
		storedFile(1, 100, 0, "G"),
		storedFile(2, 101, 0, "G"),
		storedFile(3, 102, 0, "G"),
	}, anchors)

	if partial[0].anchorMessageID != complete[0].anchorMessageID {
		t.Fatalf("anchor moved from %d to %d: the same album would be enqueued twice",
			partial[0].anchorMessageID, complete[0].anchorMessageID)
	}
	if partial[0].anchorMessageID != 100 {
		t.Fatalf("anchor = %d, want the catalogued album anchor 100", partial[0].anchorMessageID)
	}
}

// A group the catalogue read did not return keeps the previous behaviour: the
// first message seen. Degradation, never a panic or an empty anchor.
func TestGroupMediaFallsBackToTheFirstMessageWithoutAnchors(t *testing.T) {
	groups := groupMedia([]media.File{storedFile(1, 101, 0, "G")}, nil)
	if len(groups) != 1 || groups[0].anchorMessageID != 101 {
		t.Fatalf("groups = %#v, want the first message as anchor", groups)
	}
}

func TestGroupMediaOnNoFiles(t *testing.T) {
	if groups := groupMedia(nil, nil); len(groups) != 0 {
		t.Fatalf("groupMedia(nil) = %#v, want no group", groups)
	}
}
