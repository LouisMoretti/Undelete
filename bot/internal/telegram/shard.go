package telegram

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"sync"
)

const (
	// numShards bounds the worker goroutines of the update dispatcher: one
	// goroutine per shard, created once, never per update and never per
	// chat. Thirty-two partitions keep the per-partition FIFOs narrow enough
	// that a parked chat rarely shares its worker with an unrelated one,
	// while the process holds a constant, reviewable number of goroutines
	// however many tenants connect.
	numShards = 32

	// shardQueueDepth bounds the updates waiting on one shard. Together with
	// numShards it bounds the process memory held by the dispatcher:
	// numShards*(shardQueueDepth+1) updates at most, after which Dispatch
	// blocks and applies backpressure to the getUpdates loop instead of
	// growing. The depth covers several full getUpdates batches per shard so
	// a burst never stalls the fetch on a momentarily busy worker.
	shardQueueDepth = 32
)

// ShardKey returns the deterministic partition of an update: the Business
// connection plus the chat it belongs to.
//
// Every update type carrying both lands on the same partition, so the save of
// a message, the save of its edit and the handling of its deletion are
// processed by the same FIFO worker, in emission order: a deletion can never
// overtake the save it refers to. Two different chats (or connections) shard
// apart and run concurrently.
//
// A business_connection update carries no chat: it shards on the connection
// id alone. An update of no known business_* type (which the handler ignores)
// shards on its update id, spreading that negligible work instead of piling
// it on one worker.
func ShardKey(u Update) string {
	switch {
	case u.BusinessConnection != nil:
		return "conn\x00" + u.BusinessConnection.ID
	case u.BusinessMessage != nil:
		return chatKey(u.BusinessMessage.BusinessConnectionID, u.BusinessMessage.Chat.ID)
	case u.EditedBusinessMessage != nil:
		return chatKey(u.EditedBusinessMessage.BusinessConnectionID, u.EditedBusinessMessage.Chat.ID)
	case u.DeletedBusinessMessages != nil:
		return chatKey(u.DeletedBusinessMessages.BusinessConnectionID, u.DeletedBusinessMessages.Chat.ID)
	default:
		return "update\x00" + strconv.FormatInt(u.UpdateID, 10)
	}
}

func chatKey(businessConnectionID string, chatID int64) string {
	return "chat\x00" + businessConnectionID + "\x00" + strconv.FormatInt(chatID, 10)
}

// shardIndex hashes a partition key onto a worker. FNV-1a: deterministic
// across restarts (no random seed), so a partition sticks to its worker for
// the life of the process and two updates of one chat never split.
func shardIndex(key string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum64() % uint64(numShards))
}

// shardRequest is one update handed to a shard worker, with the channel its
// completion is reported on. done is buffered (size 1) so a worker never
// blocks reporting while Dispatch has already moved on to shutdown.
type shardRequest struct {
	update Update
	// ctx is the Dispatch caller's context (the poller run context): a
	// handler observes cancellation exactly as if the sequential loop had
	// called it directly, so shutdown aborts in-flight work promptly.
	ctx  context.Context
	done chan<- error
}

// ErrUpdateNotSubmitted marks an update Dispatch never submitted to its
// partition: the submit (or the wait) was aborted by context cancellation
// first. It wraps the context error. The caller must not advance its offset
// over such a slot -- no worker ever saw the update, so no server-side
// acknowledgement covers it. Slots that WERE submitted report the handler's
// own error instead (a handler observing cancellation reports a context
// error, which stays a per-update signal like any other failure).
var ErrUpdateNotSubmitted = errors.New("telegram: update never submitted")

// Dispatcher runs Handler concurrently across partitions while preserving a
// strict FIFO order inside each partition.
//
// The poller stays the single owner of the Telegram offset: it submits each
// fetched batch with Dispatch and only advances the offset once every update
// of the batch has completed (successfully or not, exactly like the former
// sequential loop). A poisoned update therefore still cannot freeze the bot,
// and a crash still redelivers from the last advanced offset with the same
// idempotent upserts as before: the durability contract is unchanged, only
// the execution overlaps.
//
// Slow-shard strategy: a parked partition delays the offset advancement of
// its own batch, but never the submission of the other partitions of that
// batch (Dispatch submits every update whose shard has room first, and only
// then blocks on the full ones). Two costs remain, both inherent to the
// single Telegram offset rather than to this dispatcher:
//
//   - per-batch barrier: the NEXT getUpdates waits for the slowest partition
//     of the CURRENT batch, so one slow tenant stalls global freshness (not
//     just its own partition) until its batch completes. Concurrent
//     partitions still overlap within the batch; the barrier only gates when
//     the next batch is fetched.
//   - bounded wait: that stall is bounded by the handler's own ceilings --
//     the command answers (10s), the welcome send (30s) and the erasure
//     (60s). A slow tenant stalls freshness, it never deadlocks the loop
//     and never loses an update.
type Dispatcher struct {
	handler Handler
	logger  *slog.Logger
	shards  []chan shardRequest
	wg      sync.WaitGroup
	stop    sync.Once
}

// NewDispatcher starts the shard workers. The handler is called from the
// workers from now on and must be safe for concurrent use; every dependency
// behind app.Handler already is (mutex-guarded connection cache, pgx pool
// with per-call InTenant transactions, stateless Telegram client, atomic
// metrics, goroutine-safe slog logger).
func NewDispatcher(handler Handler, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.New(discardHandler{})
	}
	d := &Dispatcher{handler: handler, logger: logger}
	d.shards = make([]chan shardRequest, numShards)
	for i := range d.shards {
		d.shards[i] = make(chan shardRequest, shardQueueDepth)
		d.wg.Add(1)
		go d.work(d.shards[i])
	}
	return d
}

// work drains one shard FIFO, strictly in queue order: the order guarantee of
// a partition is this loop doing one thing at a time.
func (d *Dispatcher) work(queue <-chan shardRequest) {
	defer d.wg.Done()
	for req := range queue {
		// A handler failure is per-update signal, never a worker signal: the
		// dispatcher reports it on done and moves to the next update, exactly
		// like the sequential loop logged it and advanced the offset.
		req.done <- d.serve(req)
	}
}

// serve calls the handler, converting a panic into an error. Without this a
// panicking update would kill its partition's only worker: every later
// update of that partition would pile up behind a dead queue until the
// submit blocked the whole fetch loop. A panic is still a bug to fix, so it
// is logged with the update id (never any content) and reported like any
// other handler failure: the offset advances past it.
func (d *Dispatcher) serve(req shardRequest) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			d.logger.Error("update handler panicked",
				slog.Int64("update_id", req.update.UpdateID),
				slog.String("panic", fmt.Sprint(recovered)))
			err = fmt.Errorf("update handler panicked on update %d", req.update.UpdateID)
		}
	}()
	return d.handler(req.ctx, req.update)
}

// Dispatch submits every update of one fetched batch to its partition and
// waits for all of them to complete. The returned slice is aligned with
// updates: errs[i] is the handler error for updates[i] (nil on success).
//
// The submit applies bounded backpressure to the caller (the getUpdates loop)
// instead of buffering without limit: at most numShards*(shardQueueDepth+1)
// updates are ever held, and submitting past a full shard queue blocks.
// Submission is fair: a first non-blocking pass submits every update whose
// shard has room, so a full slow shard never head-of-line-blocks the updates
// of idle shards behind it in the batch; a second pass then submits the
// deferred updates in batch order, blocking as needed. Per-partition order
// is preserved throughout: once an update of a partition is deferred, every
// later update of that same partition is deferred too, so the two passes
// concatenate in batch order on every shard.
//
// A cancelled context aborts both the submit and the wait: slots submitted
// already report the handler's own error, while updates never submitted
// report ErrUpdateNotSubmitted (wrapping the context error) and keep no
// other trace in the slice -- the caller must not advance its offset over
// them. In practice the poller returns on cancellation right after, so the
// process redelivers them on restart from the last offset Telegram acked.
func (d *Dispatcher) Dispatch(ctx context.Context, updates []Update) []error {
	errs := make([]error, len(updates))
	if len(updates) == 0 {
		return errs
	}

	pending := make([]chan error, len(updates))
	deferredReq := make([]shardRequest, len(updates))
	deferredDone := make([]chan error, len(updates))
	var deferred []int
	// deferredShard remembers the partitions already deferred in this batch:
	// trying a later update of the same partition in the first pass could
	// submit it ahead of the deferred one if the worker drained meanwhile,
	// inverting the partition order.
	deferredShard := make(map[int]bool)
	for i, u := range updates {
		done := make(chan error, 1)
		req := shardRequest{update: u, ctx: ctx, done: done}
		idx := shardIndex(ShardKey(u))
		if deferredShard[idx] {
			deferredReq[i] = req
			deferredDone[i] = done
			deferred = append(deferred, i)
			continue
		}
		select {
		case d.shards[idx] <- req:
			pending[i] = done
		case <-ctx.Done():
			for j := i; j < len(updates); j++ {
				errs[j] = fmt.Errorf("%w (update %d): %w", ErrUpdateNotSubmitted, updates[j].UpdateID, ctx.Err())
			}
			d.collect(ctx, updates, pending, errs)
			return errs
		default:
			deferredShard[idx] = true
			deferredReq[i] = req
			deferredDone[i] = done
			deferred = append(deferred, i)
		}
	}
	for pos, i := range deferred {
		select {
		case d.shards[shardIndex(ShardKey(updates[i]))] <- deferredReq[i]:
			pending[i] = deferredDone[i]
		case <-ctx.Done():
			for _, j := range deferred[pos:] {
				errs[j] = fmt.Errorf("%w (update %d): %w", ErrUpdateNotSubmitted, updates[j].UpdateID, ctx.Err())
			}
			d.collect(ctx, updates, pending, errs)
			return errs
		}
	}

	d.collect(ctx, updates, pending, errs)
	return errs
}

// collect waits for every submitted update, in batch order. Errors are the
// handler's own (including a handler observing cancellation itself); slots
// whose update was never submitted stay as the caller left them.
func (d *Dispatcher) collect(ctx context.Context, updates []Update, pending []chan error, errs []error) {
	for i := range updates {
		if pending[i] == nil {
			continue
		}
		select {
		case err := <-pending[i]:
			errs[i] = err
		case <-ctx.Done():
			for j := i; j < len(updates); j++ {
				if pending[j] == nil {
					continue
				}
				select {
				case err := <-pending[j]:
					errs[j] = err
				default:
					errs[j] = ctx.Err()
				}
			}
			return
		}
	}
}

// Stop closes the shard queues and waits for the workers to finish. Queued
// updates still drain (each runs once, against a cancelled context on
// shutdown, so handlers abort promptly); Dispatch must not be called after
// Stop. Idempotent: the poller defers it unconditionally.
func (d *Dispatcher) Stop() {
	d.stop.Do(func() {
		for _, queue := range d.shards {
			close(queue)
		}
		d.wg.Wait()
	})
}
