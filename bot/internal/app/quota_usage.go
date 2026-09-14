package app

import (
	"context"

	"github.com/LouisMoretti/Undelete/bot/internal/media"
	"github.com/LouisMoretti/Undelete/bot/internal/messages"
	"github.com/LouisMoretti/Undelete/bot/internal/quotas"
)

// quotaUsage adapts the tenant-scoped repositories to the quotas.UsageSource
// the tracker seeds and re-verifies from. It holds the concrete repositories
// (not the narrow handler interfaces): counting is not part of the save path,
// and the fakes behind messageStore/mediaCatalogue exist so handler tests run
// without a database.
//
// No SQL lives here -- only repository calls -- so the InTenant audit
// (storage/tenantsurface_test.go) is unaffected: this package still names no
// RLS table.
type quotaUsage struct {
	messages *messages.Repository
	media    *media.Repository
}

// NewQuotaUsage builds the UsageSource the quota tracker reads. Both
// repositories are the production ones wired in cmd/bot; a nil quota tracker
// (quota disabled) never touches it.
func NewQuotaUsage(messagesRepo *messages.Repository, mediaRepo *media.Repository) quotas.UsageSource {
	return quotaUsage{messages: messagesRepo, media: mediaRepo}
}

func (u quotaUsage) CountMessages(ctx context.Context, ownerUserID int64) (int64, error) {
	return u.messages.CountByOwner(ctx, ownerUserID)
}

func (u quotaUsage) CountMediaFiles(ctx context.Context, ownerUserID int64) (int64, error) {
	return u.media.CountByOwner(ctx, ownerUserID)
}

func (u quotaUsage) SumStoredMediaBytes(ctx context.Context, ownerUserID int64) (int64, error) {
	return u.media.SumStoredBytes(ctx, ownerUserID)
}
