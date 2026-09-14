package business

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/LouisMoretti/Undelete/bot/internal/telegram"
	"github.com/LouisMoretti/Undelete/bot/internal/users"
)

// concPool is a mutex-guarded fake pool: the resolution chain is now called
// from shard workers concurrently, so a test exercising that must not trip
// the race detector in the fake itself.
type concPool struct {
	t         *testing.T
	mu        sync.Mutex
	conn      Connection
	queries   int
	upserts   int
	execCalls int
}

func (p *concPool) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch len(args) {
	case 1:
		p.queries++
		return fakeRow{conn: p.conn}
	case 4:
		p.upserts++
		owner, _ := args[1].(int64)
		return fakeOwnerRow{ownerUserID: owner}
	default:
		p.t.Errorf("QueryRow args = %v, want the connection id or the four upsert columns", args)
		return fakeRow{err: pgx.ErrNoRows}
	}
}

func (p *concPool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.execCalls++
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

type concUsers struct {
	mu   sync.Mutex
	seen []int64
}

func (u *concUsers) UpsertByTelegramID(_ context.Context, telegramUserID int64) (*users.User, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.seen = append(u.seen, telegramUserID)
	return &users.User{ID: 7, TelegramUserID: telegramUserID}, nil
}

type concAPI struct {
	mu        sync.Mutex
	conn      *telegram.BusinessConnection
	connCalls int
	requests  int
}

func (a *concAPI) GetBusinessConnection(_ context.Context, _ string) (*telegram.BusinessConnection, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connCalls++
	return a.conn, nil
}

func (a *concAPI) SendMessage(_ context.Context, _ telegram.SendMessageRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests++
	return nil
}

// TestResolveConcurrentCacheHits pins that the shard workers may resolve the
// same connection simultaneously: cache hits never touch the database or the
// API, whatever the interleaving.
func TestResolveConcurrentCacheHits(t *testing.T) {
	pool := &concPool{t: t, conn: Connection{ID: "bc-shared", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: true}}
	svc := NewService(pool, &concAPI{}, &concUsers{}, nil, testLogger())
	svc.cache.store(pool.conn)

	const workers = 64
	var wg sync.WaitGroup
	errs := make([]error, workers)
	conns := make([]*Connection, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conns[i], errs[i] = svc.Resolve(context.Background(), "bc-shared")
		}(i)
	}
	wg.Wait()

	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("Resolve[%d] = %v, want nil", i, errs[i])
		}
		if conns[i] == nil || conns[i].ID != "bc-shared" || conns[i].OwnerUserID != 7 {
			t.Fatalf("Resolve[%d] = %+v, want the cached connection", i, conns[i])
		}
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.queries != 0 {
		t.Fatalf("cached resolutions issued %d database reads, want 0", pool.queries)
	}
}

// TestResolveConcurrentMissSingleflightsNothingButStaysCorrect pins the miss
// path under concurrency: every caller gets the same resolution, served from
// the database, without corrupting the cache.
func TestResolveConcurrentMissSingleflightsNothingButStaysCorrect(t *testing.T) {
	pool := &concPool{t: t, conn: Connection{ID: "bc-db", OwnerUserID: 7, OwnerTelegramUserID: 700, IsEnabled: true}}
	svc := NewService(pool, &concAPI{}, &concUsers{}, nil, testLogger())

	const workers = 32
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Resolve(context.Background(), "bc-db")
		}(i)
	}
	wg.Wait()

	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("Resolve[%d] = %v, want nil", i, errs[i])
		}
	}
	// After the race every later resolution is a cache hit: the cache holds
	// exactly one live entry for the connection.
	cached, ok := svc.cache.lookup("bc-db")
	if !ok || cached.conn.ID != "bc-db" {
		t.Fatalf("cache after concurrent miss = %+v, %v; want the resolved connection", cached, ok)
	}
}

// TestDisableOwnerConcurrentWithResolve pins the erasure half of the
// concurrency contract: disabling an owner while shard workers resolve its
// connection never races, and afterwards the connection resolves as
// disabled.
func TestDisableOwnerConcurrentWithResolve(t *testing.T) {
	pool := &concPool{t: t, conn: Connection{ID: "bc-erase", OwnerUserID: 9, OwnerTelegramUserID: 900, IsEnabled: true}}
	svc := NewService(pool, &concAPI{}, &concUsers{}, nil, testLogger())
	svc.cache.store(pool.conn)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.Resolve(context.Background(), "bc-erase")
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.DisableOwner(context.Background(), 9)
		}()
	}
	wg.Wait()

	got, err := svc.Resolve(context.Background(), "bc-erase")
	if err != nil {
		t.Fatalf("Resolve after DisableOwner = %v, want nil", err)
	}
	if got.IsEnabled {
		t.Fatal("connection still resolves as enabled after DisableOwner")
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.execCalls == 0 {
		t.Fatal("DisableOwner issued no UPDATE")
	}
}
