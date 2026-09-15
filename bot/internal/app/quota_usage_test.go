package app

import (
	"testing"

	"github.com/LouisMoretti/Undelete/bot/internal/quotas"
)

// The adapter's unit-testable contract: quotaUsage satisfies
// quotas.UsageSource, the interface cmd/bot wires into quotas.NewTracker
// (main.go). The three delegate methods are one-line forwards into
// *messages.Repository / *media.Repository, whose pool only exists against
// real Postgres: the struct holds the concrete repository types precisely so
// no fake can stand behind the counting path. The tracker's seeding from a
// UsageSource is covered by the quotas package (TestSeedsFromSourceOnFirstTouch
// and friends); the forwarding itself is a production seam, exercised by the
// integration recipes, not by unit tests.
var _ quotas.UsageSource = quotaUsage{}

// TestNewQuotaUsageReturnsUsageSource pins the constructor: even handed nil
// repositories it returns a usable UsageSource value, never a nil interface.
// A regression to returning nil would silently switch the wired tracker onto
// its nil-source fail-open path (quotas.resync treats a nil source as
// "seeded, count nothing"), i.e. quotas enforced by nothing.
func TestNewQuotaUsageReturnsUsageSource(t *testing.T) {
	if src := NewQuotaUsage(nil, nil); src == nil {
		t.Fatal("NewQuotaUsage(nil, nil) = nil, want a non-nil quotas.UsageSource")
	}
}
