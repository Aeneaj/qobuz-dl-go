package downloader

import (
	"context"
	"sync"
)

// batchOutcome is how one entry of a track batch ended.
type batchOutcome int

const (
	notReached  batchOutcome = iota // cancelled before its turn
	fetched                         // downloaded now, or already on disk
	lookupError                     // the id could not be looked up
	notFound                        // the lookup found nothing to fetch
	failed                          // the track could not be prepared or downloaded
)

// batchResult is one entry's outcome and, unless fetched or notReached, why.
type batchResult struct {
	outcome batchOutcome
	err     error
}

// downloadTrackBatch downloads n loose tracks into baseDir through the same
// two-stage pipeline as an album: one resolver goroutine looks up each id
// (idAt) and prepares its track in order, and d.Opts.Workers goroutines
// download. It returns one result per entry, in entry order.
//
// Playlists and CSV imports went one track at a time — --workers did not
// apply to them. The resolver stays single so that everything before the
// bytes stays sequential: two tracks of one album share a cover, and the
// cover must be on disk before either is tagged.
//
// idAt returns "" with no error when there is nothing to fetch for entry i.
func (d *Downloader) downloadTrackBatch(ctx context.Context, n int, idAt func(ctx context.Context, i int) (string, error), baseDir string) []batchResult {
	results := make([]batchResult, n)
	workers := max(1, d.Opts.Workers)

	p := d.newProgress(ctx)
	restore := d.withBars(p)
	defer restore()

	type readyTrack struct {
		i int
		t *looseTrack
	}
	ready := make(chan readyTrack, workers)

	// Each entry's result is written by exactly one goroutine — the resolver
	// or the worker it handed the entry to — and read after both are done.
	go func() {
		defer close(ready)
		for i := range n {
			if ctx.Err() != nil {
				return
			}
			id, err := idAt(ctx, i)
			if ctx.Err() != nil {
				return
			}
			if err == nil && id == "" {
				results[i] = batchResult{outcome: notFound}
				continue
			}
			if err != nil {
				results[i] = batchResult{lookupError, err}
				continue
			}
			t, err := d.prepareTrack(ctx, id, baseDir, i+1)
			if ctx.Err() != nil {
				return // stopping is not a failure to report
			}
			switch {
			case err != nil:
				results[i] = batchResult{failed, err}
				continue
			case t == nil:
				results[i] = batchResult{outcome: fetched}
				continue
			}
			select {
			case ready <- readyTrack{i, t}:
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for r := range ready {
				if ctx.Err() != nil {
					continue
				}
				switch err := d.fetchTrack(ctx, r.t, p); {
				case ctx.Err() != nil:
					// stopped: left notReached
				case err != nil:
					results[r.i] = batchResult{failed, err}
				default:
					results[r.i] = batchResult{outcome: fetched}
				}
			}
		})
	}
	wg.Wait()
	if p != nil {
		p.Wait()
	}
	return results
}
