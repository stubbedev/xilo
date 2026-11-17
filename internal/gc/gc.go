// Package gc is the one garbage-collection routine, shared by the server's
// background sweep, the dashboard and API triggers, and a local `xilo gc`:
// evict by age, retention, plan ceilings and storage caps, keep each local
// backend above its free-space floor, sweep the chunks nothing references any
// more, and refresh the stored summaries of every cache that lost paths.
package gc

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/storage"
	"github.com/stubbedev/xilo/internal/store"
)

// Sweep is what one run enforces.
type Sweep struct {
	Policy config.GCPolicy
	// ConfiguredCap is limits.total; the instance rule overrides it when set.
	ConfiguredCap int64
	// OlderThan additionally evicts every path not pulled within it.
	OlderThan time.Duration
}

// Result is what one run removed.
type Result struct {
	Evicted int
	Deleted int
	Freed   int64
}

// Run evicts, then sweeps every backend in sts.
func Run(ctx context.Context, db *store.DB, sts map[string]storage.Storage, sw Sweep, now time.Time) (Result, error) {
	evicted := store.Evicted{}
	res, err := run(ctx, db, sts, sw, now, evicted)
	res.Evicted = evicted.Total()
	if refreshErr := refreshStats(db, evicted); err == nil {
		err = refreshErr
	}
	return res, err
}

func run(ctx context.Context, db *store.DB, sts map[string]storage.Storage, sw Sweep, now time.Time, evicted store.Evicted) (Result, error) {
	var res Result
	if sw.OlderThan > 0 {
		e, err := db.EvictPathsOlderThan(now.Add(-sw.OlderThan).Unix())
		if err != nil {
			return res, fmt.Errorf("evict paths older than %s: %w", sw.OlderThan, err)
		}
		evicted.Merge(e)
	}
	if err := enforceCaches(db, sw.Policy.Retention, now, evicted); err != nil {
		return res, err
	}
	instanceCap := db.InstanceCap(sw.ConfiguredCap)
	e, err := db.EnforceGlobalCap(instanceCap)
	if err != nil {
		return res, fmt.Errorf("enforce instance cap of %d bytes: %w", instanceCap, err)
	}
	evicted.Merge(e)

	graceCutoff := now.Add(-sw.Policy.Grace).Unix()
	for _, name := range slices.Sorted(maps.Keys(sts)) {
		st := sts[name]
		if err := keepFloor(db, name, st, sw.Policy.MinFree, evicted); err != nil {
			return res, err
		}
		d, f, err := db.GC(ctx, st, name, graceCutoff)
		res.Deleted += d
		res.Freed += f
		if err != nil {
			return res, fmt.Errorf("sweep chunks on storage %q: %w", name, err)
		}
	}
	return res, nil
}

func enforceCaches(db *store.DB, globalRetention time.Duration, now time.Time, evicted store.Evicted) error {
	caches, err := db.ListCaches()
	if err != nil {
		return fmt.Errorf("list caches: %w", err)
	}
	ceilings := map[int64]time.Duration{}
	for _, c := range caches {
		if _, ok := ceilings[c.AccountID]; ok {
			continue
		}
		lim, err := retentionCeiling(db, c.AccountID)
		if err != nil {
			return err
		}
		ceilings[c.AccountID] = lim
	}
	for _, c := range caches {
		ret := globalRetention
		if c.Retention > 0 {
			ret = time.Duration(c.Retention) * time.Second
		}
		if lim := ceilings[c.AccountID]; lim > 0 && (ret == 0 || ret > lim) {
			ret = lim
		}
		if ret > 0 {
			e, err := db.EvictCachePathsOlderThan(c.ID, now.Add(-ret).Unix())
			if err != nil {
				return fmt.Errorf("evict paths of %s/%s past retention %s: %w", c.Account, c.Name, ret, err)
			}
			evicted.Merge(e)
		}
		e, err := db.EnforceCacheCap(c.ID, c.MaxBytes)
		if err != nil {
			return fmt.Errorf("enforce cap of %d bytes on %s/%s: %w", c.MaxBytes, c.Account, c.Name, err)
		}
		evicted.Merge(e)
	}
	return nil
}

func retentionCeiling(db *store.DB, accountID int64) (time.Duration, error) {
	acc, err := db.GetAccountByID(accountID)
	if err != nil {
		return 0, fmt.Errorf("load account %d for its plan's retention ceiling: %w", accountID, err)
	}
	plan, err := db.AccountPlan(acc)
	if err != nil {
		return 0, fmt.Errorf("load plan of account %d: %w", accountID, err)
	}
	if plan == nil || plan.MaxRetention <= 0 {
		return 0, nil
	}
	return time.Duration(plan.MaxRetention) * time.Second, nil
}

func keepFloor(db *store.DB, name string, st storage.Storage, floor config.FreeFloor, evicted store.Evicted) error {
	if !floor.Set() {
		return nil
	}
	sr, ok := st.(storage.SpaceReporter)
	if !ok {
		return nil
	}
	sp, err := sr.Space()
	if err != nil {
		return fmt.Errorf("measure free space of storage %q: %w", name, err)
	}
	want := floor.Bytes(sp.Total)
	if sp.Free >= want {
		return nil
	}
	need := int64(want - sp.Free)
	e, err := db.FreeOnStorage(name, need)
	if err != nil {
		return fmt.Errorf("free %d bytes on storage %q: %w", need, name, err)
	}
	evicted.Merge(e)
	return nil
}

func refreshStats(db *store.DB, evicted store.Evicted) error {
	for _, id := range slices.Sorted(maps.Keys(evicted)) {
		if _, err := db.RefreshStats(id); err != nil {
			return fmt.Errorf("refresh stats of cache %d: %w", id, err)
		}
	}
	return nil
}
