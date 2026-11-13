package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/store"
)

// catchUp the cache with the log: apply the commit objects between the position that the cache
// records and the head of the manifest, oldest first, each in one transaction of the cache. It
// finds them by walking the log back from the head, because a commit object names only the one
// before it. It renews the lease on the way when a renewal is due. It requires the writer lock and
// the manifest lock, and that the instance holds the lease.
//
// It stops the instance for what can never be right by trying again: a cache whose position is
// not in the log, with [ErrCacheNotInLog]; a commit object that needs a newer version of s4ql, is
// malformed, or whose bytes do not match its name after a second download, with the error of the
// reader; and a commit object that is not in the store or does not fit the cache, with
// [ErrLogBroken]. It never skips a commit object and never rebuilds the cache. A download that
// fails for another reason, such as one of the store, is tried again where it failed, so that a
// long log is caught up also when not every download of it succeeds in a row. It fails only when
// the instance stops, loses the lease, or ctx is done.
func (d *DB) catchUp(ctx context.Context) (err error) {
	clock := d.stopwatch()
	from, err := d.w.position()
	if err != nil {
		return d.cacheError(ctx, "catch up", err)
	}
	head := d.man.Head
	if from == head {
		return nil
	}

	var missing []format.Name
	var applied int
	var bytes int64
	defer func() {
		outcome, level := "caught_up", slog.LevelInfo
		if err != nil {
			outcome, level = "failed", slog.LevelWarn
			if errors.Is(d.stoppedError(), ErrClosed) {
				// A close ended it, which is no failure.
				level = slog.LevelInfo
			}
		}
		d.event(ctx, level, eventCatchUp, slog.String(attrOutcome, outcome), slog.String(attrCatchUpFrom, from.String()),
			slog.String(attrCatchUpTo, head.String()), slog.Int(attrCatchUpCommits, applied), slog.Int64(attrBytes, bytes), clock.total(), errAttr(err))
	}()

	// Each commit object is read to its end here, so that its parent is known to be what its
	// writer wrote, and read once more when it is applied.
	for name := head; name != from; {
		if name == format.EmptyLog {
			return d.stop(ctx, fmt.Errorf("%w: the cache is at %v, and the log that ends at %v does not hold it", ErrCacheNotInLog, from, head))
		}
		if err := d.keepLease(ctx); err != nil {
			return err
		}
		var parent format.Name
		if err := d.fetchAgain(ctx, name, func(body io.Reader) error {
			var err error
			parent, err = parentOf(name, body)
			return err
		}); errors.Is(err, format.ErrNameMismatch) && d.bugs.skipUnreadable {
			// Under the planted bug, a commit object is skipped only when it is applied.
			continue
		} else if err != nil {
			return err
		}
		missing = append(missing, name)
		name = parent
	}

	for i := len(missing) - 1; i >= 0; i-- {
		name := missing[i]
		err := d.fetchAgain(ctx, name, func(body io.Reader) error {
			done, err := replay(d.w, name, body, func() error { return d.keepLease(ctx) }, d.bugs)
			bytes += done.bytes
			return err
		})
		if d.bugs.skipUnreadable && errors.Is(err, format.ErrNameMismatch) {
			// The planted bug: the commit object counts as applied.
			err = d.w.setPosition(name)
		}
		if err != nil {
			return err
		}
		applied++
	}
	return nil
}

// parentOf the commit object with the given name, read from body to its end, so that the bytes
// are known to match the name.
func parentOf(name format.Name, body io.Reader) (format.Name, error) {
	r := format.NewCommitReader(body, name)
	header, err := r.Header()
	if err != nil {
		return format.Name{}, err
	}
	for {
		if _, _, err := r.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return format.Name{}, err
		}
	}
	return format.NameFromBytes(header.GetParent())
}

// fetchAgain fetches the commit object with the given name as [DB.fetch] does, and again for as
// long as it fails for a reason that another try can cure, with a longer wait after each failure
// and a renewal of the lease when one is due. It requires the manifest lock.
func (d *DB) fetchAgain(ctx context.Context, name format.Name, use func(body io.Reader) error) error {
	wait := retrySoon
	for {
		err := d.fetch(ctx, name, use)
		if err == nil || errors.Is(err, ErrStopped) || errors.Is(err, ErrNoLease) || (errors.Is(err, format.ErrNameMismatch) && d.bugs.skipUnreadable) {
			return err
		}
		if sleepErr := d.host.Sleep(ctx, wait); sleepErr != nil {
			return after(sleepErr, err)
		}
		wait = min(2*wait, max(retryAfter, d.options.Watch))
		if err := d.keepLease(ctx); err != nil {
			return err
		}
	}
}

// fetch the commit object with the given name from the store and hand its content to use, as
// [DB.download] does. It stops the instance for a commit object that can never be read, by
// [unreadable], and for a failure of the cache. It passes on any other error.
func (d *DB) fetch(ctx context.Context, name format.Name, use func(body io.Reader) error) error {
	err := d.download(ctx, name, use)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, format.ErrNameMismatch) && d.bugs.skipUnreadable:
		return err
	case unreadable(err):
		return d.stop(ctx, err)
	}
	return d.cacheError(ctx, "catch up", err)
}

// unreadable reports whether err says that a commit object can never be read, or never be applied:
// it needs a newer version of s4ql, it is malformed, its bytes do not match its name, the store
// does not have it, or it does not fit the cache.
func unreadable(err error) bool {
	return errors.Is(err, format.ErrNameMismatch) || errors.Is(err, format.ErrNewerVersion) || errors.Is(err, format.ErrMalformed) || errors.Is(err, ErrLogBroken)
}

// download the commit object with the given name from the store and hand its content to use. If
// use reports that the bytes do not match the name, it downloads the commit object once more,
// because the bytes may have been damaged on their way. A commit object that the store does not
// have is an error that wraps [ErrLogBroken].
func (d *DB) download(ctx context.Context, name format.Name, use func(body io.Reader) error) error {
	err := d.fetchOnce(ctx, name, use)
	if errors.Is(err, format.ErrNameMismatch) && !d.bugs.skipUnreadable {
		err = d.fetchOnce(ctx, name, use)
	}
	if errors.Is(err, store.ErrNotFound) {
		return brokenf("the commit object %v is not in the store", name)
	}
	return err
}

// fetchOnce downloads the commit object with the given name and hands its content to use.
func (d *DB) fetchOnce(ctx context.Context, name format.Name, use func(body io.Reader) error) error {
	rctx, cancel := d.host.WithTimeout(ctx, d.options.ObjectTimeout)
	defer cancel()
	body, _, err := d.store.Get(rctx, d.commitKey(name))
	if err != nil {
		return err
	}
	err = use(body)
	_ = body.Close()
	return err
}
