package erasure

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeChallenges reimplements data_erasure_requests in memory. The SQL half is
// covered by the integration test against a real PostgreSQL; what needs
// covering here is the DECISION each state leads to, since every one of them
// is a decision about destroying a tenant's data.
type fakeChallenges struct {
	rows map[string]*fakeRequest
	// issueErr, claimErr and completeErr inject a database failure at each of
	// the three points where one changes the outcome.
	issueErr, claimErr, completeErr error
	deleted                         []string
}

type fakeRequest struct {
	owner     int64
	status    string
	expiresAt time.Time
}

func newChallenges() *fakeChallenges {
	return &fakeChallenges{rows: map[string]*fakeRequest{}}
}

func (f *fakeChallenges) Issue(_ context.Context, t Tenant, codeHash string, ttl time.Duration) (time.Time, error) {
	if f.issueErr != nil {
		return time.Time{}, f.issueErr
	}
	for hash, row := range f.rows {
		if row.owner == t.OwnerUserID && row.status == "pending" {
			delete(f.rows, hash)
		}
	}
	expires := time.Now().Add(ttl)
	f.rows[codeHash] = &fakeRequest{owner: t.OwnerUserID, status: "pending", expiresAt: expires}
	return expires, nil
}

func (f *fakeChallenges) Claim(_ context.Context, ownerUserID int64, codeHash string) (ClaimState, error) {
	if f.claimErr != nil {
		return ClaimUnknown, f.claimErr
	}
	row, ok := f.rows[codeHash]
	// The owner check mirrors both the WHERE clause and the RLS policy: a code
	// of another tenant is indistinguishable from one that never existed.
	if !ok || row.owner != ownerUserID {
		return ClaimUnknown, nil
	}
	switch row.status {
	case "pending":
		if !row.expiresAt.After(time.Now()) {
			return ClaimExpired, nil
		}
		row.status = "consumed"
		return ClaimGranted, nil
	case "consumed":
		return ClaimResumable, nil
	default:
		return ClaimCompleted, nil
	}
}

func (f *fakeChallenges) Complete(_ context.Context, ownerUserID int64, codeHash string) error {
	if f.completeErr != nil {
		return f.completeErr
	}
	if row, ok := f.rows[codeHash]; ok && row.owner == ownerUserID && row.status == "consumed" {
		row.status = "completed"
	}
	return nil
}

func (f *fakeChallenges) DeleteOthers(_ context.Context, ownerUserID int64, keepHash string) (int64, error) {
	var deleted int64
	for hash, row := range f.rows {
		if row.owner == ownerUserID && hash != keepHash {
			delete(f.rows, hash)
			deleted++
		}
	}
	f.deleted = append(f.deleted, keepHash)
	return deleted, nil
}

// fakeSteps records the deletion steps in the order they ran, which is the
// property the package comment rests on, and can fail at a chosen one.
type fakeSteps struct {
	calls   []string
	failAt  string
	failErr error
}

func (f *fakeSteps) record(step string) error {
	f.calls = append(f.calls, step)
	if f.failAt == step {
		if f.failErr == nil {
			f.failErr = errors.New("step failed")
		}
		return f.failErr
	}
	return nil
}

func (f *fakeSteps) DisableOwner(_ context.Context, _ int64) (int64, error) {
	return 1, f.record("connections")
}

func (f *fakeSteps) DeleteTenant(_ context.Context, _ int64) (int64, error) {
	return 2, f.record("outbox")
}

func (f *fakeSteps) EraseTenant(_ context.Context, _ int64) (int64, int64, error) {
	return 3, 4, f.record("media")
}

// messageSteps exists only because Messages and Outbox would otherwise collide
// on the same method name in one fake.
type messageSteps struct{ steps *fakeSteps }

func (m messageSteps) DeleteTenant(_ context.Context, _ int64) (int64, int64, error) {
	return 5, 6, m.steps.record("messages")
}

func newService(t *testing.T, challenges Challenges, steps *fakeSteps) *Service {
	t.Helper()
	s, err := New(Config{
		Challenges:  challenges,
		Connections: steps,
		Outbox:      steps,
		Media:       steps,
		Messages:    messageSteps{steps: steps},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

var testTenant = Tenant{OwnerUserID: 11, OwnerTelegramUserID: 700001, BusinessConnectionID: "bc-1"}

// TestRequestThenConfirmErases is the happy path, and it pins the ORDER of the
// steps: the connections are disabled FIRST, so no step behind it can race a
// message being saved, and the messages go last because the blobs are located
// from rows that must still exist when they are unlinked.
func TestRequestThenConfirmErases(t *testing.T) {
	challenges := newChallenges()
	steps := &fakeSteps{}
	service := newService(t, challenges, steps)

	challenge, err := service.Request(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if challenge.Code == "" {
		t.Fatal("no code issued")
	}
	if !challenge.ExpiresAt.After(time.Now()) {
		t.Fatal("the issued code is already expired")
	}

	outcome, err := service.Confirm(context.Background(), testTenant, challenge.Code)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if outcome != OutcomeErased {
		t.Fatalf("outcome = %v, want OutcomeErased", outcome)
	}

	want := []string{"connections", "outbox", "media", "messages"}
	if strings.Join(steps.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("steps ran as %v, want %v", steps.calls, want)
	}
}

// TestTheCodeIsNeverStored: what reaches the store is a hash. A dump handed to
// anyone -- and every row here travels into every pg_dump -- must not contain a
// spendable erasure token.
func TestTheCodeIsNeverStored(t *testing.T) {
	challenges := newChallenges()
	service := newService(t, challenges, &fakeSteps{})

	challenge, err := service.Request(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	for hash := range challenges.rows {
		if strings.Contains(strings.ToUpper(hash), challenge.Code) {
			t.Fatal("the stored value contains the code itself")
		}
		if len(hash) != 64 {
			t.Fatalf("stored value is %d characters, want a 64-character sha256 hex", len(hash))
		}
	}
}

// TestReplayedCodeErasesNothingMore is the idempotence criterion: the same code
// submitted a second time must neither fail nor rerun the deletion.
func TestReplayedCodeErasesNothingMore(t *testing.T) {
	challenges := newChallenges()
	steps := &fakeSteps{}
	service := newService(t, challenges, steps)

	challenge, err := service.Request(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if _, err := service.Confirm(context.Background(), testTenant, challenge.Code); err != nil {
		t.Fatalf("first Confirm: %v", err)
	}
	stepsAfterFirst := len(steps.calls)

	outcome, err := service.Confirm(context.Background(), testTenant, challenge.Code)
	if err != nil {
		t.Fatalf("second Confirm must not fail: %v", err)
	}
	if outcome != OutcomeAlreadyErased {
		t.Fatalf("outcome = %v, want OutcomeAlreadyErased", outcome)
	}
	if len(steps.calls) != stepsAfterFirst {
		t.Fatalf("the replay ran %d more deletion steps, want 0", len(steps.calls)-stepsAfterFirst)
	}
}

// TestExpiredCodeIsRefused: the window is what keeps a code left on a screen,
// or in the history of a monitored chat, from being a standing authorisation.
func TestExpiredCodeIsRefused(t *testing.T) {
	challenges := newChallenges()
	steps := &fakeSteps{}
	service := newService(t, challenges, steps)

	challenge, err := service.Request(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	challenges.rows[HashCode(challenge.Code)].expiresAt = time.Now().Add(-time.Second)

	outcome, err := service.Confirm(context.Background(), testTenant, challenge.Code)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if outcome != OutcomeExpired {
		t.Fatalf("outcome = %v, want OutcomeExpired", outcome)
	}
	if len(steps.calls) != 0 {
		t.Fatalf("an expired code ran %v", steps.calls)
	}
}

// TestUnknownCodesAreRefused covers the three ways a submitted code can fail to
// designate anything: never issued, issued to another tenant, or empty.
func TestUnknownCodesAreRefused(t *testing.T) {
	challenges := newChallenges()
	steps := &fakeSteps{}
	service := newService(t, challenges, steps)

	issued, err := service.Request(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	other := Tenant{OwnerUserID: 22, OwnerTelegramUserID: 700002, BusinessConnectionID: "bc-2"}
	tests := []struct {
		name   string
		tenant Tenant
		code   string
	}{
		{name: "a code nobody issued", tenant: testTenant, code: "ZZZZZZZZ"},
		{name: "another tenant's code", tenant: other, code: issued.Code},
		{name: "no code at all", tenant: testTenant, code: "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, err := service.Confirm(context.Background(), tt.tenant, tt.code)
			if err != nil {
				t.Fatalf("Confirm: %v", err)
			}
			if outcome != OutcomeUnknown {
				t.Fatalf("outcome = %v, want OutcomeUnknown", outcome)
			}
			if len(steps.calls) != 0 {
				t.Fatalf("a refused code ran %v", steps.calls)
			}
		})
	}
}

// TestOnlyOneOfTwoSubmissionsErases is the single-use guarantee seen from the
// service: the first submission spends the row, and the second finds a state
// the store itself moved it into. Nothing here relies on the two arriving in
// any particular order -- what decides is that Claim is one atomic write.
func TestOnlyOneOfTwoSubmissionsErases(t *testing.T) {
	challenges := newChallenges()
	steps := &fakeSteps{}
	service := newService(t, challenges, steps)

	challenge, err := service.Request(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	granted := 0
	for range 2 {
		state, err := challenges.Claim(context.Background(), testTenant.OwnerUserID, HashCode(challenge.Code))
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if state == ClaimGranted {
			granted++
		}
	}
	if granted != 1 {
		t.Fatalf("%d submissions were granted, want exactly 1", granted)
	}
}

// TestInterruptedErasureResumes: a crash between two steps leaves the request
// consumed, and the same code resubmitted reruns the whole list. Rerunning is
// the point -- every step is idempotent -- and the alternative, refusing the
// code, would strand a tenant halfway through an erasure with no way to finish
// it.
func TestInterruptedErasureResumes(t *testing.T) {
	challenges := newChallenges()
	steps := &fakeSteps{failAt: "media"}
	service := newService(t, challenges, steps)

	challenge, err := service.Request(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	if _, err := service.Confirm(context.Background(), testTenant, challenge.Code); err == nil {
		t.Fatal("a failing step must surface as an error")
	}
	if got := strings.Join(steps.calls, ","); got != "connections,outbox,media" {
		t.Fatalf("steps ran as %q: the failure must stop the list", got)
	}
	if status := challenges.rows[HashCode(challenge.Code)].status; status != "consumed" {
		t.Fatalf("request status = %q, want %q so the erasure stays resumable", status, "consumed")
	}

	steps.failAt = ""
	steps.calls = nil
	outcome, err := service.Confirm(context.Background(), testTenant, challenge.Code)
	if err != nil {
		t.Fatalf("resumed Confirm: %v", err)
	}
	if outcome != OutcomeErased {
		t.Fatalf("outcome = %v, want OutcomeErased", outcome)
	}
	if got := strings.Join(steps.calls, ","); got != "connections,outbox,media,messages" {
		t.Fatalf("the resumed run did %q, want every step from the beginning", got)
	}
	if status := challenges.rows[HashCode(challenge.Code)].status; status != "completed" {
		t.Fatalf("request status = %q after a successful resume, want %q", status, "completed")
	}
}

// TestAFailedFirstStepDeletesNothing: if the connections cannot be disabled,
// nothing else runs. Deleting a tenant's messages while their connection is
// still capturing new ones is the one ordering this package exists to prevent.
func TestAFailedFirstStepDeletesNothing(t *testing.T) {
	challenges := newChallenges()
	steps := &fakeSteps{failAt: "connections"}
	service := newService(t, challenges, steps)

	challenge, err := service.Request(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if _, err := service.Confirm(context.Background(), testTenant, challenge.Code); err == nil {
		t.Fatal("a failing first step must surface as an error")
	}
	if got := strings.Join(steps.calls, ","); got != "connections" {
		t.Fatalf("steps ran as %q, want only the first one", got)
	}
}

// TestIssuingACodeInvalidatesThePreviousOne: one live code at a time, so an
// older message still sitting in the chat history cannot authorise anything.
func TestIssuingACodeInvalidatesThePreviousOne(t *testing.T) {
	challenges := newChallenges()
	steps := &fakeSteps{}
	service := newService(t, challenges, steps)

	first, err := service.Request(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("first Request: %v", err)
	}
	second, err := service.Request(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("second Request: %v", err)
	}
	if first.Code == second.Code {
		t.Fatal("two requests issued the same code")
	}

	outcome, err := service.Confirm(context.Background(), testTenant, first.Code)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if outcome != OutcomeUnknown {
		t.Fatalf("the superseded code yielded %v, want OutcomeUnknown", outcome)
	}
	if len(steps.calls) != 0 {
		t.Fatalf("the superseded code ran %v", steps.calls)
	}
}

// TestTheCompletedRequestSurvivesTheErasure: everything of the tenant goes,
// except the receipt that makes a replay answerable. That is the one row the
// erasure deliberately keeps, and the test states which.
func TestTheCompletedRequestSurvivesTheErasure(t *testing.T) {
	challenges := newChallenges()
	service := newService(t, challenges, &fakeSteps{})

	stale, err := service.Request(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	// A second tenant's row must survive too: the erasure is tenant-scoped.
	other := Tenant{OwnerUserID: 22, OwnerTelegramUserID: 700002}
	if _, err := service.Request(context.Background(), other); err != nil {
		t.Fatalf("other tenant Request: %v", err)
	}
	spent, err := service.Request(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if _, err := service.Confirm(context.Background(), testTenant, spent.Code); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	if _, ok := challenges.rows[HashCode(spent.Code)]; !ok {
		t.Fatal("the spent request is gone: a replayed confirmation would read as an unknown code")
	}
	if _, ok := challenges.rows[HashCode(stale.Code)]; ok {
		t.Fatal("a superseded request of the erased tenant survived")
	}
	var otherRows int
	for _, row := range challenges.rows {
		if row.owner == other.OwnerUserID {
			otherRows++
		}
	}
	if otherRows != 1 {
		t.Fatalf("the other tenant has %d requests left, want 1: the erasure is tenant-scoped", otherRows)
	}
}

// TestStoreFailuresSurface: a database that cannot record the challenge, claim
// it or complete it must produce an error, never a silent success. A confirmed
// erasure that was not recorded would be replayable.
func TestStoreFailuresSurface(t *testing.T) {
	t.Run("issue fails", func(t *testing.T) {
		challenges := newChallenges()
		challenges.issueErr = errors.New("database down")
		service := newService(t, challenges, &fakeSteps{})
		if _, err := service.Request(context.Background(), testTenant); err == nil {
			t.Fatal("Request returned no error")
		}
	})

	t.Run("claim fails", func(t *testing.T) {
		challenges := newChallenges()
		steps := &fakeSteps{}
		service := newService(t, challenges, steps)
		challenges.claimErr = errors.New("database down")
		if _, err := service.Confirm(context.Background(), testTenant, "ABCD2345"); err == nil {
			t.Fatal("Confirm returned no error")
		}
		if len(steps.calls) != 0 {
			t.Fatalf("a failed claim ran %v", steps.calls)
		}
	})

	t.Run("complete fails", func(t *testing.T) {
		challenges := newChallenges()
		steps := &fakeSteps{}
		service := newService(t, challenges, steps)
		challenge, err := service.Request(context.Background(), testTenant)
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		challenges.completeErr = errors.New("database down")
		if _, err := service.Confirm(context.Background(), testTenant, challenge.Code); err == nil {
			t.Fatal("Confirm returned no error although the request could not be completed")
		}
	})
}

// TestNewRequiresEveryDependency: a Service missing one of them would report an
// erasure it did not perform.
func TestNewRequiresEveryDependency(t *testing.T) {
	steps := &fakeSteps{}
	full := Config{
		Challenges:  newChallenges(),
		Connections: steps,
		Outbox:      steps,
		Media:       steps,
		Messages:    messageSteps{steps: steps},
	}
	tests := []struct {
		name   string
		break_ func(*Config)
	}{
		{name: "challenges", break_: func(c *Config) { c.Challenges = nil }},
		{name: "connections", break_: func(c *Config) { c.Connections = nil }},
		{name: "outbox", break_: func(c *Config) { c.Outbox = nil }},
		{name: "media", break_: func(c *Config) { c.Media = nil }},
		{name: "messages", break_: func(c *Config) { c.Messages = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := full
			tt.break_(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatalf("New accepted a configuration without %s", tt.name)
			}
		})
	}
	if _, err := New(full); err != nil {
		t.Fatalf("New refused a complete configuration: %v", err)
	}
}

// TestNormaliseCodeFoldsWhatAClientDoes: the owner retypes the code by hand as
// often as they copy it, and a code refused over a lower-case letter or a dash
// would send them back for a new one for no reason.
func TestNormaliseCodeFoldsWhatAClientDoes(t *testing.T) {
	const canonical = "ABCD2345"
	for _, typed := range []string{"ABCD2345", "abcd2345", " ABCD2345 ", "ABCD-2345", "ABCD 2345", "abcd_2345"} {
		if got := NormaliseCode(typed); got != canonical {
			t.Fatalf("NormaliseCode(%q) = %q, want %q", typed, got, canonical)
		}
		if HashCode(typed) != HashCode(canonical) {
			t.Fatalf("%q hashes differently from %q", typed, canonical)
		}
	}
	// What normalisation must NOT do: make two different codes equal.
	if HashCode("ABCD2345") == HashCode("ABCD2346") {
		t.Fatal("two different codes share a hash")
	}
}

// TestGeneratedCodesUseTheUnambiguousAlphabet guards the two properties of the
// generator that are easy to lose in an edit: the alphabet (no character a
// reader confuses with another) and the length. The unbiased mapping in
// newCode depends on the alphabet having exactly 32 symbols.
func TestGeneratedCodesUseTheUnambiguousAlphabet(t *testing.T) {
	if len(codeAlphabet) != 32 {
		t.Fatalf("alphabet has %d symbols: the modulo mapping in newCode is only unbiased for a divisor of 256", len(codeAlphabet))
	}
	for _, forbidden := range []string{"I", "O", "0", "1"} {
		if strings.Contains(codeAlphabet, forbidden) {
			t.Fatalf("the alphabet contains %q, which readers confuse with another symbol", forbidden)
		}
	}

	seen := map[string]bool{}
	for range 200 {
		code, err := newCode()
		if err != nil {
			t.Fatalf("newCode: %v", err)
		}
		if len(code) != codeLength {
			t.Fatalf("code %q is %d symbols, want %d", code, len(code), codeLength)
		}
		for _, r := range code {
			if !strings.ContainsRune(codeAlphabet, r) {
				t.Fatalf("code %q contains %q, outside the alphabet", code, r)
			}
		}
		seen[code] = true
	}
	// Not a statistical test, just the assertion that the generator is drawing
	// at all: a constant or a counter would collapse this to a handful.
	if len(seen) < 190 {
		t.Fatalf("200 draws produced %d distinct codes: the generator is not random enough", len(seen))
	}
}

// TestChallengeTTLStaysShort: the window is a security property, not a comfort
// setting. The confirmation is typed in a chat a contact reads, so a TTL
// stretched to hours would leave a live erasure token in their view.
func TestChallengeTTLStaysShort(t *testing.T) {
	if ChallengeTTL <= 0 || ChallengeTTL > 15*time.Minute {
		t.Fatalf("ChallengeTTL = %v, want a short positive window", ChallengeTTL)
	}
}
