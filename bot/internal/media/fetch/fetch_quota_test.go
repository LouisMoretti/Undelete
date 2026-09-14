package fetch

import (
	"context"
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/media/store"
	"github.com/LouisMoretti/Undelete/bot/internal/quotas"
)

func quotaLimitsForTest() quotas.Limits {
	return quotas.Limits{
		MaxMessages:       100000,
		MaxMediaFiles:     100000,
		MaxMediaBytes:     100,
		CapturesPerMinute: 100000,
		WarnPercent:       50,
	}
}

func mustQuotaTracker(t *testing.T, limits quotas.Limits) *quotas.Tracker {
	t.Helper()
	tr, err := quotas.NewTracker(limits, nil, nil)
	if err != nil {
		t.Fatalf("NewTracker: %v", err)
	}
	return tr
}

// TestQuotaRefusedDownloadIsCataloguedWithoutFile pins the bytes behaviour:
// past the quota the download never starts and the row is marked purged --
// the deletion alert can still say a media existed, and the loop stops asking
// for it, exactly like a file Telegram will never hand over.
func TestQuotaRefusedDownloadIsCataloguedWithoutFile(t *testing.T) {
	cat := &fakeCatalogue{pending: []media.File{pendingFile(1), pendingFile(2)}}
	dl := &fakeDownloader{saved: testSaved(1024)}
	f := testFetcher(cat, nil, dl)
	WithQuota(mustQuotaTracker(t, quotaLimitsForTest()))(f)

	stored, err := f.ProcessTenant(context.Background(), 11)
	if err != nil {
		t.Fatalf("ProcessTenant: %v", err)
	}
	if stored != 1 {
		t.Fatalf("stored = %d, want 1: the second download is past the quota", stored)
	}
	if dl.calls != 1 {
		t.Fatalf("downloads = %d, want 1: the refused file must never hit the network", dl.calls)
	}
	if len(cat.purged) != 1 || cat.purged[0] != 2 {
		t.Fatalf("purged = %v, want [2]: the refused row stays catalogued without a file", cat.purged)
	}
	if len(cat.stored) != 1 {
		t.Fatalf("stored rows = %d, want 1", len(cat.stored))
	}
}

// TestSaturatedTenantKeepsOthersDownloading pins the isolation: a tenant past
// its byte quota has its downloads catalogued without a file, while another
// tenant's downloads proceed on the same tracker.
func TestSaturatedTenantKeepsOthersDownloading(t *testing.T) {
	ctx := context.Background()
	tr := mustQuotaTracker(t, quotaLimitsForTest())
	if adm := tr.AddMediaBytes(ctx, 11, 100); !adm.Allowed {
		t.Fatalf("seeding tenant 11: %+v", adm)
	}

	catSaturated := &fakeCatalogue{pending: []media.File{pendingFile(1)}}
	dlSaturated := &fakeDownloader{saved: testSaved(64)}
	saturated := testFetcher(catSaturated, nil, dlSaturated)
	WithQuota(tr)(saturated)

	catFree := &fakeCatalogue{pending: []media.File{pendingFile(2)}}
	dlFree := &fakeDownloader{saved: testSaved(64)}
	free := testFetcher(catFree, nil, dlFree)
	WithQuota(tr)(free)

	if stored, err := saturated.ProcessTenant(ctx, 11); err != nil || stored != 0 {
		t.Fatalf("saturated ProcessTenant = (%d, %v), want (0, nil)", stored, err)
	}
	if dlSaturated.calls != 0 {
		t.Fatalf("saturated downloads = %d, want 0", dlSaturated.calls)
	}
	if stored, err := free.ProcessTenant(ctx, 22); err != nil || stored != 1 {
		t.Fatalf("free ProcessTenant = (%d, %v), want (1, nil)", stored, err)
	}
}

func testSaved(bytes int64) store.StoredFile {
	return store.StoredFile{
		RelPath: "11/2026-01/01/quota-test",
		SHA256:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Bytes:   bytes,
	}
}
