package store

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTenantsShareOneMediaRootUnderDistinctSubtrees pins the property the
// media backup depends on once the deployment holds several tenants: every
// tenant's blobs live under DISTINCT subtrees of the SAME configured root.
//
// scripts/backup-media.sh archives that single root. If a tenant's files could
// land outside it -- a second root per tenant, an absolute path, a traversal --
// the archive would be silently partial, and the gap would only be discovered
// at restore time, when the missing files are the ones nobody has any more.
// Distinct subtrees matter for the other half of the promise: an erasure sweeps
// one tenant's tree, and two tenants sharing a directory would make that sweep
// either incomplete or too wide.
func TestTenantsShareOneMediaRootUnderDistinctSubtrees(t *testing.T) {
	const root = "media"
	fixed := time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)
	downloader, err := New(Config{BaseDir: root, Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatalf("new downloader: %v", err)
	}

	owners := []int64{7, 8, 9}
	prefixes := make(map[string]int64, len(owners))
	for _, ownerUserID := range owners {
		rel, err := downloader.relPath(Request{OwnerUserID: ownerUserID, UniqueID: UniqueID("unique-file")})
		if err != nil {
			t.Fatalf("relPath for tenant %d: %v", ownerUserID, err)
		}
		// Relative to the root, so one archive of that root covers it.
		if strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
			t.Fatalf("tenant %d stores outside the media root: %q", ownerUserID, rel)
		}
		// Resolving it must stay inside the root, which is the check the
		// production path applies before writing a single byte.
		if _, err := downloader.resolve(rel); err != nil {
			t.Fatalf("tenant %d path escapes the root: %v", ownerUserID, err)
		}

		prefix, _, found := strings.Cut(rel, "/")
		if !found {
			t.Fatalf("tenant %d path %q has no tenant directory", ownerUserID, rel)
		}
		if prefix != strconv.FormatInt(ownerUserID, 10) {
			t.Fatalf("tenant %d stores under %q, want its own id as the first component", ownerUserID, prefix)
		}
		if previous, clash := prefixes[prefix]; clash {
			t.Fatalf("tenants %d and %d share the subtree %q", previous, ownerUserID, prefix)
		}
		prefixes[prefix] = ownerUserID
	}

	if len(prefixes) != len(owners) {
		t.Fatalf("%d tenants produced %d subtrees", len(owners), len(prefixes))
	}
}
