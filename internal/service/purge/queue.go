// Package purge implements a durable, at-least-once delete queue for
// permanently removing storage content. The Rust API owns all trash
// semantics (what's deleted, when, and any grace period) in its own
// metadata store; by the time it calls Enqueue here with a batch of
// storage paths, deletion is a foregone conclusion for every one of them.
// The only job left for the Storage API is making sure those bytes are
// actually gone — even if this process crashes or restarts mid-way —
// without making the caller wait for disk I/O.
//
// Every path is persisted to a local SQLite database (see db.go) before
// Enqueue returns, so a caller only needs that call to succeed to know
// the deletion is durably recorded — it will survive this process being
// killed outright. A pool of background workers then removes the
// underlying files and clears each path's row only once its file is
// confirmed gone. On startup, Start loads whatever rows are still
// there — left over from a previous run that didn't finish — and resumes
// exactly where it left off; a periodic sweep does the same during
// normal operation, so a failed delete (or a path dropped because the
// internal work channel was momentarily full) is retried automatically
// instead of being silently lost.
//
// A path ending in "/" is a recursive delete of everything under it (an
// entire org, user, or folder), not a single file. Those are walked and
// deleted one file at a time within a single worker — deliberately not
// via a bulk os.RemoveAll — so a huge org/user delete can never generate
// more concurrent disk I/O than the worker pool already allows for
// ordinary single-file deletes; see processPrefix.
package purge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Yukthi-Systems/YFS-Storage-API/internal/storage"
)

const (
	// deleteTimeout bounds a single file deletion, independent of any
	// HTTP request that originally enqueued it.
	deleteTimeout = 30 * time.Second
	// sweepInterval controls how often the queue re-reads the database
	// for anything still pending, which is also the worst-case retry
	// delay for a failed delete.
	sweepInterval = 60 * time.Second
)

// Queue durably tracks and executes permanent file deletions.
type Queue struct {
	db      *sql.DB
	store   storage.Storage
	logger  *slog.Logger
	workers int

	workCh   chan string
	inFlight sync.Map // path (string) -> struct{}
}

// New builds a Queue backed by db (see OpenDB) and store. workers is the
// number of concurrent goroutines performing deletions; it is clamped to
// at least 1.
func New(db *sql.DB, store storage.Storage, logger *slog.Logger, workers int) *Queue {
	if workers < 1 {
		workers = 1
	}
	return &Queue{
		db:      db,
		store:   store,
		logger:  logger,
		workers: workers,
		workCh:  make(chan string, 4096),
	}
}

// Start launches the worker pool and periodic sweep, and resumes any
// deletions left pending from a previous run (e.g. a crash or restart
// between Enqueue persisting a path and it actually being removed). It
// returns once that backlog has been loaded from the database and handed
// to the workers; it does not wait for them to finish. Workers and the
// sweep stop when ctx is cancelled.
func (q *Queue) Start(ctx context.Context) error {
	for i := 0; i < q.workers; i++ {
		go q.worker(ctx)
	}
	go q.sweepLoop(ctx)

	n, err := q.resync(ctx)
	if err != nil {
		return fmt.Errorf("purge: loading pending deletes on startup: %w", err)
	}
	if n > 0 {
		q.logger.Info("purge: resumed pending deletes from previous run", "count", n)
	}
	return nil
}

// Enqueue durably records paths for permanent deletion and returns once
// they are committed to disk. Callers (an HTTP handler) may respond to
// their own caller as soon as this returns — actual filesystem deletion
// happens asynchronously and is guaranteed to complete eventually,
// including across a service restart. Re-enqueuing a path already
// pending is a no-op for that path.
func (q *Queue) Enqueue(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	for _, p := range paths {
		if p == "" {
			return errors.New("purge: path must not be empty")
		}
	}

	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("purge: beginning enqueue transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO pending_deletes (path, enqueued_at) VALUES (?, ?) ON CONFLICT(path) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("purge: preparing enqueue statement: %w", err)
	}
	defer stmt.Close()

	now := time.Now().Unix()
	for _, p := range paths {
		if _, err := stmt.ExecContext(ctx, p, now); err != nil {
			return fmt.Errorf("purge: persisting pending delete %q: %w", p, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("purge: committing enqueue transaction: %w", err)
	}

	for _, p := range paths {
		q.submit(p)
	}
	return nil
}

// submit hands path to a worker if one is free and the path isn't already
// in flight. It never blocks: if the work channel is full, the path stays
// recorded on disk and the next sweep picks it up.
func (q *Queue) submit(path string) {
	if _, loaded := q.inFlight.LoadOrStore(path, struct{}{}); loaded {
		return
	}
	select {
	case q.workCh <- path:
	default:
		q.inFlight.Delete(path)
	}
}

func (q *Queue) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case path := <-q.workCh:
			q.process(path)
			q.inFlight.Delete(path)
		}
	}
}

func (q *Queue) process(path string) {
	if isPrefix(path) {
		q.processPrefix(path)
		return
	}
	q.processFile(path)
}

// isPrefix reports whether path names a recursive delete target (an
// entire org, user, or folder) rather than a single file, by the same
// trailing-"/" convention the Rust API uses when it builds the batch.
func isPrefix(path string) bool {
	return strings.HasSuffix(path, "/")
}

func (q *Queue) processFile(path string) {
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()

	if err := q.store.Delete(ctx, path); err != nil {
		q.logger.Error("purge: delete failed, will retry", "path", path, "error", err)
		q.recordFailure(path, err)
		return
	}
	q.clearPending(path)
}

// processPrefix recursively deletes everything under prefix by walking it
// and deleting one file at a time, sequentially, within this single
// worker goroutine — never via storage.Storage.DeletePrefix's bulk
// os.RemoveAll, which would unlink as fast as the OS allows with no
// pacing at all. Keeping it sequential per worker, combined with the
// queue's fixed worker pool already capping how many deletes run at once
// across the whole service, is what keeps a bulk org/user/folder delete
// from spiking disk I/O beyond what ordinary single-file deletes already
// bound it to — no separate rate limiter needed.
//
// If the walk is interrupted (a delete error, or the queue shutting
// down), the prefix's row stays pending and the next sweep re-walks it,
// picking up wherever it left off: re-deleting an already-gone file is a
// no-op, so resuming is just re-listing what's left.
func (q *Queue) processPrefix(prefix string) {
	ctx := context.Background()

	err := q.store.ListPrefix(ctx, prefix, func(key string) error {
		dctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
		defer cancel()
		return q.store.Delete(dctx, key)
	})
	if err != nil {
		q.logger.Error("purge: recursive delete failed, will retry", "prefix", prefix, "error", err)
		q.recordFailure(prefix, err)
		return
	}

	// Every file under prefix is gone; what's left is empty directories,
	// which DeletePrefix's os.RemoveAll clears in one cheap call — no
	// file content left to unlink, so no I/O spike here either.
	dctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()
	if err := q.store.DeletePrefix(dctx, prefix); err != nil {
		q.logger.Error("purge: failed to clean up empty directories, will retry", "prefix", prefix, "error", err)
		q.recordFailure(prefix, err)
		return
	}
	q.clearPending(prefix)
}

// clearPending deletes path's row once its file is confirmed gone.
// Failing to clear it is harmless: the next sweep will just find the
// (already-deleted) path still marked pending, retry the now-no-op
// delete, and try clearing it again.
func (q *Queue) clearPending(path string) {
	if _, err := q.db.ExecContext(context.Background(), `DELETE FROM pending_deletes WHERE path = ?`, path); err != nil {
		q.logger.Error("purge: delete succeeded but failed to clear pending row; will retry harmlessly", "path", path, "error", err)
	}
}

// recordFailure notes a failed attempt against path's row (for
// monitoring — e.g. `SELECT * FROM pending_deletes WHERE attempts > 0`)
// without removing it; the row staying present is what makes the next
// sweep retry it.
func (q *Queue) recordFailure(path string, cause error) {
	_, err := q.db.ExecContext(context.Background(),
		`UPDATE pending_deletes SET attempts = attempts + 1, last_error = ?, last_attempt_at = ? WHERE path = ?`,
		cause.Error(), time.Now().Unix(), path)
	if err != nil {
		q.logger.Error("purge: failed to record delete failure", "path", path, "error", err)
	}
}

// sweepLoop periodically resubmits whatever is still recorded as
// pending, so a failed delete or a path dropped under backpressure is
// retried without waiting for a restart.
func (q *Queue) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := q.resync(ctx); err != nil {
				q.logger.Error("purge: sweep failed", "error", err)
			}
		}
	}
}

// resync submits every path currently marked pending and returns how many
// it found.
func (q *Queue) resync(ctx context.Context) (int, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT path FROM pending_deletes`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return count, err
		}
		q.submit(path)
		count++
	}
	return count, rows.Err()
}
