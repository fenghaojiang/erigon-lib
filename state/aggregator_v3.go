/*
   Copyright 2022 Erigon contributors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package state

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	math2 "math"
	"runtime"
	"strings"
	"time"

	"github.com/RoaringBitmap/roaring/roaring64"
	"github.com/ledgerwatch/erigon-lib/common/dbg"
	"github.com/ledgerwatch/erigon-lib/kv/order"
	"github.com/ledgerwatch/log/v3"
	"go.uber.org/atomic"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	common2 "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/cmp"
	"github.com/ledgerwatch/erigon-lib/etl"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/bitmapdb"
)

type AggregatorV3 struct {
	rwTx             kv.RwTx
	db               kv.RoDB
	storage          *History
	tracesTo         *InvertedIndex
	backgroundResult *BackgroundResult
	code             *History
	logAddrs         *InvertedIndex
	logTopics        *InvertedIndex
	tracesFrom       *InvertedIndex
	accounts         *History
	logPrefix        string
	dir              string
	tmpdir           string
	txNum            atomic.Uint64
	aggregationStep  uint64
	keepInDB         uint64
	maxTxNum         atomic.Uint64

	working                atomic.Bool
	workingMerge           atomic.Bool
	workingOptionalIndices atomic.Bool
	warmupWorking          atomic.Bool
	ctx                    context.Context
	ctxCancel              context.CancelFunc
}

func NewAggregatorV3(ctx context.Context, dir, tmpdir string, aggregationStep uint64, db kv.RoDB) (*AggregatorV3, error) {
	ctx, ctxCancel := context.WithCancel(ctx)
	a := &AggregatorV3{ctx: ctx, ctxCancel: ctxCancel, dir: dir, tmpdir: tmpdir, aggregationStep: aggregationStep, backgroundResult: &BackgroundResult{}, db: db, keepInDB: 2 * aggregationStep}
	return a, nil
}

func (a *AggregatorV3) ReopenFiles() error {
	dir := a.dir
	aggregationStep := a.aggregationStep
	var err error
	if a.accounts, err = NewHistory(dir, a.tmpdir, aggregationStep, "accounts", kv.AccountHistoryKeys, kv.AccountIdx, kv.AccountHistoryVals, kv.AccountSettings, false /* compressVals */, nil); err != nil {
		return fmt.Errorf("ReopenFiles: %w", err)
	}
	if a.storage, err = NewHistory(dir, a.tmpdir, aggregationStep, "storage", kv.StorageHistoryKeys, kv.StorageIdx, kv.StorageHistoryVals, kv.StorageSettings, false /* compressVals */, nil); err != nil {
		return fmt.Errorf("ReopenFiles: %w", err)
	}
	if a.code, err = NewHistory(dir, a.tmpdir, aggregationStep, "code", kv.CodeHistoryKeys, kv.CodeIdx, kv.CodeHistoryVals, kv.CodeSettings, true /* compressVals */, nil); err != nil {
		return fmt.Errorf("ReopenFiles: %w", err)
	}
	if a.logAddrs, err = NewInvertedIndex(dir, a.tmpdir, aggregationStep, "logaddrs", kv.LogAddressKeys, kv.LogAddressIdx, false, nil); err != nil {
		return fmt.Errorf("ReopenFiles: %w", err)
	}
	if a.logTopics, err = NewInvertedIndex(dir, a.tmpdir, aggregationStep, "logtopics", kv.LogTopicsKeys, kv.LogTopicsIdx, false, nil); err != nil {
		return fmt.Errorf("ReopenFiles: %w", err)
	}
	if a.tracesFrom, err = NewInvertedIndex(dir, a.tmpdir, aggregationStep, "tracesfrom", kv.TracesFromKeys, kv.TracesFromIdx, false, nil); err != nil {
		return fmt.Errorf("ReopenFiles: %w", err)
	}
	if a.tracesTo, err = NewInvertedIndex(dir, a.tmpdir, aggregationStep, "tracesto", kv.TracesToKeys, kv.TracesToIdx, false, nil); err != nil {
		return fmt.Errorf("ReopenFiles: %w", err)
	}
	a.recalcMaxTxNum()
	return nil
}

func (a *AggregatorV3) Close() {
	a.ctxCancel()
	a.closeFiles()
}

func (a *AggregatorV3) SetWorkers(i int) {
	a.accounts.workers = i
	a.storage.workers = i
	a.code.workers = i
	a.logAddrs.workers = i
	a.logTopics.workers = i
	a.tracesFrom.workers = i
	a.tracesTo.workers = i
}

func (a *AggregatorV3) Files() (res []string) {
	res = append(res, a.accounts.Files()...)
	res = append(res, a.storage.Files()...)
	res = append(res, a.code.Files()...)
	res = append(res, a.logAddrs.Files()...)
	res = append(res, a.logTopics.Files()...)
	res = append(res, a.tracesFrom.Files()...)
	res = append(res, a.tracesTo.Files()...)
	return res
}

func (a *AggregatorV3) closeFiles() {
	if a.accounts != nil {
		a.accounts.Close()
	}
	if a.storage != nil {
		a.storage.Close()
	}
	if a.code != nil {
		a.code.Close()
	}
	if a.logAddrs != nil {
		a.logAddrs.Close()
	}
	if a.logTopics != nil {
		a.logTopics.Close()
	}
	if a.tracesFrom != nil {
		a.tracesFrom.Close()
	}
	if a.tracesTo != nil {
		a.tracesTo.Close()
	}
}

func (a *AggregatorV3) BuildOptionalMissedIndices(ctx context.Context) {
	if a.workingOptionalIndices.Load() {
		return
	}
	a.workingOptionalIndices.Store(true)
	go func() {
		defer a.workingOptionalIndices.Store(false)

		//It's time to build optional lazy indices

		if err := a.accounts.localityIndex.BuildMissedIndices(ctx, a.accounts.InvertedIndex); err != nil {
			log.Warn("merge", "err", err)
		}
		if err := a.storage.localityIndex.BuildMissedIndices(ctx, a.storage.InvertedIndex); err != nil {
			log.Warn("merge", "err", err)
		}
		if err := a.code.localityIndex.BuildMissedIndices(ctx, a.code.InvertedIndex); err != nil {
			log.Warn("merge", "err", err)
		}
	}()
}

func (a *AggregatorV3) BuildMissedIndices(ctx context.Context, sem *semaphore.Weighted) error {
	if err := a.storage.localityIndex.BuildMissedIndices(ctx, a.storage.InvertedIndex); err != nil {
		panic(err)
	}
	if err := a.accounts.localityIndex.BuildMissedIndices(ctx, a.accounts.InvertedIndex); err != nil {
		return err
	}
	if err := a.code.localityIndex.BuildMissedIndices(ctx, a.code.InvertedIndex); err != nil {
		return err
	}
	g, ctx := errgroup.WithContext(ctx)
	if a.accounts != nil {
		g.Go(func() error { return a.accounts.BuildMissedIndices(ctx, sem) })
		g.Go(func() error { return a.accounts.localityIndex.BuildMissedIndices(ctx, a.accounts.InvertedIndex) })
	}
	if a.storage != nil {
		g.Go(func() error { return a.storage.BuildMissedIndices(ctx, sem) })
		g.Go(func() error { return a.storage.localityIndex.BuildMissedIndices(ctx, a.storage.InvertedIndex) })
	}
	if a.code != nil {
		g.Go(func() error { return a.code.BuildMissedIndices(ctx, sem) })
		g.Go(func() error { return a.code.localityIndex.BuildMissedIndices(ctx, a.code.InvertedIndex) })
	}
	if a.logAddrs != nil {
		g.Go(func() error { return a.logAddrs.BuildMissedIndices(ctx, sem) })
	}
	if a.logTopics != nil {
		g.Go(func() error { return a.logTopics.BuildMissedIndices(ctx, sem) })
	}
	if a.tracesFrom != nil {
		g.Go(func() error { return a.tracesFrom.BuildMissedIndices(ctx, sem) })
	}
	if a.tracesTo != nil {
		g.Go(func() error { return a.tracesTo.BuildMissedIndices(ctx, sem) })
	}

	return g.Wait()
}

func (a *AggregatorV3) SetLogPrefix(v string) { a.logPrefix = v }

func (a *AggregatorV3) SetTx(tx kv.RwTx) {
	a.rwTx = tx
	a.accounts.SetTx(tx)
	a.storage.SetTx(tx)
	a.code.SetTx(tx)
	a.logAddrs.SetTx(tx)
	a.logTopics.SetTx(tx)
	a.tracesFrom.SetTx(tx)
	a.tracesTo.SetTx(tx)
}

func (a *AggregatorV3) SetTxNum(txNum uint64) {
	a.txNum.Store(txNum)
	a.accounts.SetTxNum(txNum)
	a.storage.SetTxNum(txNum)
	a.code.SetTxNum(txNum)
	a.logAddrs.SetTxNum(txNum)
	a.logTopics.SetTxNum(txNum)
	a.tracesFrom.SetTxNum(txNum)
	a.tracesTo.SetTxNum(txNum)
}

type AggV3Collation struct {
	logAddrs   map[string]*roaring64.Bitmap
	logTopics  map[string]*roaring64.Bitmap
	tracesFrom map[string]*roaring64.Bitmap
	tracesTo   map[string]*roaring64.Bitmap
	accounts   HistoryCollation
	storage    HistoryCollation
	code       HistoryCollation
}

func (c AggV3Collation) Close() {
	c.accounts.Close()
	c.storage.Close()
	c.code.Close()

	for _, b := range c.logAddrs {
		bitmapdb.ReturnToPool64(b)
	}
	for _, b := range c.logTopics {
		bitmapdb.ReturnToPool64(b)
	}
	for _, b := range c.tracesFrom {
		bitmapdb.ReturnToPool64(b)
	}
	for _, b := range c.tracesTo {
		bitmapdb.ReturnToPool64(b)
	}
}

func (a *AggregatorV3) buildFiles(ctx context.Context, step uint64, txFrom, txTo uint64, db kv.RoDB) (Agg22StaticFiles, error) {
	logEvery := time.NewTicker(60 * time.Second)
	defer logEvery.Stop()
	defer func(t time.Time) {
		log.Info(fmt.Sprintf("[snapshot] build %d-%d", step, step+1), "took", time.Since(t))
	}(time.Now())
	var sf Agg22StaticFiles
	var ac AggV3Collation
	closeColl := true
	defer func() {
		if closeColl {
			ac.Close()
		}
	}()
	//var wg sync.WaitGroup
	//wg.Add(7)
	//errCh := make(chan error, 7)
	//go func() {
	//	defer wg.Done()
	var err error
	if err = db.View(ctx, func(tx kv.Tx) error {
		ac.accounts, err = a.accounts.collate(step, txFrom, txTo, tx, logEvery)
		return err
	}); err != nil {
		return sf, err
		//errCh <- err
	}

	if sf.accounts, err = a.accounts.buildFiles(ctx, step, ac.accounts); err != nil {
		return sf, err
		//errCh <- err
	}
	//}()
	//
	//go func() {
	//	defer wg.Done()
	//	var err error
	if err = db.View(ctx, func(tx kv.Tx) error {
		ac.storage, err = a.storage.collate(step, txFrom, txTo, tx, logEvery)
		return err
	}); err != nil {
		return sf, err
		//errCh <- err
	}

	if sf.storage, err = a.storage.buildFiles(ctx, step, ac.storage); err != nil {
		return sf, err
		//errCh <- err
	}
	//}()
	//go func() {
	//	defer wg.Done()
	//	var err error
	if err = db.View(ctx, func(tx kv.Tx) error {
		ac.code, err = a.code.collate(step, txFrom, txTo, tx, logEvery)
		return err
	}); err != nil {
		return sf, err
		//errCh <- err
	}

	if sf.code, err = a.code.buildFiles(ctx, step, ac.code); err != nil {
		return sf, err
		//errCh <- err
	}
	//}()
	//go func() {
	//	defer wg.Done()
	//	var err error
	if err = db.View(ctx, func(tx kv.Tx) error {
		ac.logAddrs, err = a.logAddrs.collate(ctx, txFrom, txTo, tx, logEvery)
		return err
	}); err != nil {
		return sf, err
		//errCh <- err
	}

	if sf.logAddrs, err = a.logAddrs.buildFiles(ctx, step, ac.logAddrs); err != nil {
		return sf, err
		//errCh <- err
	}
	//}()
	//go func() {
	//	defer wg.Done()
	//	var err error
	if err = db.View(ctx, func(tx kv.Tx) error {
		ac.logTopics, err = a.logTopics.collate(ctx, txFrom, txTo, tx, logEvery)
		return err
	}); err != nil {
		return sf, err
		//errCh <- err
	}

	if sf.logTopics, err = a.logTopics.buildFiles(ctx, step, ac.logTopics); err != nil {
		return sf, err
		//errCh <- err
	}
	//}()
	//go func() {
	//	defer wg.Done()
	//	var err error
	if err = db.View(ctx, func(tx kv.Tx) error {
		ac.tracesFrom, err = a.tracesFrom.collate(ctx, txFrom, txTo, tx, logEvery)
		return err
	}); err != nil {
		return sf, err
		//errCh <- err
	}

	if sf.tracesFrom, err = a.tracesFrom.buildFiles(ctx, step, ac.tracesFrom); err != nil {
		return sf, err
		//errCh <- err
	}
	//}()
	//go func() {
	//	defer wg.Done()
	//	var err error
	if err = db.View(ctx, func(tx kv.Tx) error {
		ac.tracesTo, err = a.tracesTo.collate(ctx, txFrom, txTo, tx, logEvery)
		return err
	}); err != nil {
		return sf, err
		//errCh <- err
	}

	if sf.tracesTo, err = a.tracesTo.buildFiles(ctx, step, ac.tracesTo); err != nil {
		return sf, err
		//		errCh <- err
	}
	//}()
	//go func() {
	//	wg.Wait()
	//close(errCh)
	//}()
	//var lastError error
	//for err := range errCh {
	//	if err != nil {
	//		lastError = err
	//	}
	//}
	//if lastError == nil {
	closeColl = false
	//}
	return sf, nil
}

type Agg22StaticFiles struct {
	accounts   HistoryFiles
	storage    HistoryFiles
	code       HistoryFiles
	logAddrs   InvertedFiles
	logTopics  InvertedFiles
	tracesFrom InvertedFiles
	tracesTo   InvertedFiles
}

func (sf Agg22StaticFiles) Close() {
	sf.accounts.Close()
	sf.storage.Close()
	sf.code.Close()
	sf.logAddrs.Close()
	sf.logTopics.Close()
	sf.tracesFrom.Close()
	sf.tracesTo.Close()
}

func (a *AggregatorV3) BuildFiles(ctx context.Context, db kv.RoDB) (err error) {
	if (a.txNum.Load() + 1) <= a.maxTxNum.Load()+a.aggregationStep+a.keepInDB { // Leave one step worth in the DB
		return nil
	}

	// trying to create as much small-step-files as possible:
	// - to reduce amount of small merges
	// - to remove old data from db as early as possible
	// - during files build, may happen commit of new data. on each loop step getting latest id in db
	step := a.EndTxNumMinimax() / a.aggregationStep
	for ; step < lastIdInDB(db, a.accounts.indexKeysTable)/a.aggregationStep; step++ {
		if err := a.buildFilesInBackground(ctx, step, db); err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Warn("buildFilesInBackground", "err", err)
			}
			break
		}
	}
	return nil
}

func (a *AggregatorV3) buildFilesInBackground(ctx context.Context, step uint64, db kv.RoDB) (err error) {
	closeAll := true
	log.Info("[snapshots] history build", "step", fmt.Sprintf("%d-%d", step, step+1))
	sf, err := a.buildFiles(ctx, step, step*a.aggregationStep, (step+1)*a.aggregationStep, db)
	if err != nil {
		return err
	}
	defer func() {
		if closeAll {
			sf.Close()
		}
	}()
	a.integrateFiles(sf, step*a.aggregationStep, (step+1)*a.aggregationStep)

	closeAll = false
	return nil
}

func (a *AggregatorV3) mergeLoopStep(ctx context.Context, workers int) (somethingDone bool, err error) {
	closeAll := true
	maxSpan := a.aggregationStep * StepsInBiggestFile
	r := a.findMergeRange(a.maxTxNum.Load(), maxSpan)
	if !r.any() {
		return false, nil
	}

	outs := a.staticFilesInRange(r)
	defer func() {
		if closeAll {
			outs.Close()
		}
	}()
	in, err := a.mergeFiles(ctx, outs, r, maxSpan, workers)
	if err != nil {
		return true, err
	}
	defer func() {
		if closeAll {
			in.Close()
		}
	}()
	a.integrateMergedFiles(outs, in)
	if err = a.deleteFiles(outs); err != nil {
		return true, err
	}
	closeAll = false
	return true, nil
}
func (a *AggregatorV3) MergeLoop(ctx context.Context, workers int) error {
	for {
		somethingMerged, err := a.mergeLoopStep(ctx, workers)
		if err != nil {
			return err
		}
		if !somethingMerged {
			return nil
		}
	}
}

func (a *AggregatorV3) integrateFiles(sf Agg22StaticFiles, txNumFrom, txNumTo uint64) {
	a.accounts.integrateFiles(sf.accounts, txNumFrom, txNumTo)
	a.storage.integrateFiles(sf.storage, txNumFrom, txNumTo)
	a.code.integrateFiles(sf.code, txNumFrom, txNumTo)
	a.logAddrs.integrateFiles(sf.logAddrs, txNumFrom, txNumTo)
	a.logTopics.integrateFiles(sf.logTopics, txNumFrom, txNumTo)
	a.tracesFrom.integrateFiles(sf.tracesFrom, txNumFrom, txNumTo)
	a.tracesTo.integrateFiles(sf.tracesTo, txNumFrom, txNumTo)
	a.recalcMaxTxNum()
}

func (a *AggregatorV3) Unwind(ctx context.Context, txUnwindTo uint64, stateLoad etl.LoadFunc) error {
	stateChanges := etl.NewCollector(a.logPrefix, a.tmpdir, etl.NewOldestEntryBuffer(etl.BufferOptimalSize))
	defer stateChanges.Close()
	if err := a.accounts.pruneF(txUnwindTo, math2.MaxUint64, func(_ uint64, k, v []byte) error {
		return stateChanges.Collect(k, v)
	}); err != nil {
		return err
	}
	if err := a.storage.pruneF(txUnwindTo, math2.MaxUint64, func(_ uint64, k, v []byte) error {
		return stateChanges.Collect(k, v)
	}); err != nil {
		return err
	}

	if err := stateChanges.Load(a.rwTx, kv.PlainState, stateLoad, etl.TransformArgs{Quit: ctx.Done()}); err != nil {
		return err
	}
	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	if err := a.logAddrs.prune(ctx, txUnwindTo, math2.MaxUint64, math2.MaxUint64, logEvery); err != nil {
		return err
	}
	if err := a.logTopics.prune(ctx, txUnwindTo, math2.MaxUint64, math2.MaxUint64, logEvery); err != nil {
		return err
	}
	if err := a.tracesFrom.prune(ctx, txUnwindTo, math2.MaxUint64, math2.MaxUint64, logEvery); err != nil {
		return err
	}
	if err := a.tracesTo.prune(ctx, txUnwindTo, math2.MaxUint64, math2.MaxUint64, logEvery); err != nil {
		return err
	}
	return nil
}

func (a *AggregatorV3) Warmup(ctx context.Context, txFrom, limit uint64) {
	if a.db == nil {
		return
	}
	if limit < 10_000 {
		return
	}
	if a.warmupWorking.Load() {
		return
	}
	a.warmupWorking.Store(true)
	go func() {
		defer a.warmupWorking.Store(false)
		if err := a.db.View(ctx, func(tx kv.Tx) error {
			if err := a.accounts.warmup(ctx, txFrom, limit, tx); err != nil {
				return err
			}
			if err := a.storage.warmup(ctx, txFrom, limit, tx); err != nil {
				return err
			}
			if err := a.code.warmup(ctx, txFrom, limit, tx); err != nil {
				return err
			}
			if err := a.logAddrs.warmup(txFrom, limit, tx); err != nil {
				return err
			}
			if err := a.logTopics.warmup(txFrom, limit, tx); err != nil {
				return err
			}
			if err := a.tracesFrom.warmup(txFrom, limit, tx); err != nil {
				return err
			}
			if err := a.tracesTo.warmup(txFrom, limit, tx); err != nil {
				return err
			}
			return nil
		}); err != nil {
			log.Warn("[snapshots] prune warmup", "err", err)
		}
	}()
}

// StartWrites - pattern: `defer agg.StartWrites().FinishWrites()`
func (a *AggregatorV3) DiscardHistory() *AggregatorV3 {
	a.accounts.DiscardHistory(a.tmpdir)
	a.storage.DiscardHistory(a.tmpdir)
	a.code.DiscardHistory(a.tmpdir)
	a.logAddrs.DiscardHistory(a.tmpdir)
	a.logTopics.DiscardHistory(a.tmpdir)
	a.tracesFrom.DiscardHistory(a.tmpdir)
	a.tracesTo.DiscardHistory(a.tmpdir)
	return a
}

// StartWrites - pattern: `defer agg.StartWrites().FinishWrites()`
func (a *AggregatorV3) StartWrites() *AggregatorV3 {
	a.accounts.StartWrites(a.tmpdir)
	a.storage.StartWrites(a.tmpdir)
	a.code.StartWrites(a.tmpdir)
	a.logAddrs.StartWrites(a.tmpdir)
	a.logTopics.StartWrites(a.tmpdir)
	a.tracesFrom.StartWrites(a.tmpdir)
	a.tracesTo.StartWrites(a.tmpdir)
	return a
}
func (a *AggregatorV3) FinishWrites() {
	a.accounts.FinishWrites()
	a.storage.FinishWrites()
	a.code.FinishWrites()
	a.logAddrs.FinishWrites()
	a.logTopics.FinishWrites()
	a.tracesFrom.FinishWrites()
	a.tracesTo.FinishWrites()
}

type flusher interface {
	Flush(ctx context.Context, tx kv.RwTx) error
}

func (a *AggregatorV3) Flush(ctx context.Context, tx kv.RwTx) error {
	flushers := []flusher{
		a.accounts.Rotate(),
		a.storage.Rotate(),
		a.code.Rotate(),
		a.logAddrs.Rotate(),
		a.logTopics.Rotate(),
		a.tracesFrom.Rotate(),
		a.tracesTo.Rotate(),
	}
	defer func(t time.Time) { log.Debug("[snapshots] history flush", "took", time.Since(t)) }(time.Now())
	for _, f := range flushers {
		if err := f.Flush(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

func (a *AggregatorV3) CanPrune(tx kv.Tx) bool { return a.CanPruneFrom(tx) < a.maxTxNum.Load() }
func (a *AggregatorV3) CanPruneFrom(tx kv.Tx) uint64 {
	fst, _ := kv.FirstKey(tx, kv.TracesToKeys)
	fst2, _ := kv.FirstKey(tx, kv.StorageHistoryKeys)
	if len(fst) > 0 && len(fst2) > 0 {
		fstInDb := binary.BigEndian.Uint64(fst)
		fstInDb2 := binary.BigEndian.Uint64(fst2)
		return cmp.Min(fstInDb, fstInDb2)
	}
	return math2.MaxUint64
}

func (a *AggregatorV3) PruneWithTiemout(ctx context.Context, timeout time.Duration) error {
	t := time.Now()
	for a.CanPrune(a.rwTx) && time.Since(t) < timeout {
		if err := a.Prune(ctx, 1_000); err != nil { // prune part of retired data, before commit
			return err
		}
	}
	return nil
}

func (a *AggregatorV3) Prune(ctx context.Context, limit uint64) error {
	//ctx, cancel := context.WithCancel(ctx)
	//defer cancel()
	//go func() {
	//	a.Warmup(ctx, 0, cmp.Max(a.aggregationStep, limit)) // warmup is asyn and moving faster than data deletion
	//}()
	return a.prune(ctx, 0, a.maxTxNum.Load(), limit)
}

func (a *AggregatorV3) prune(ctx context.Context, txFrom, txTo, limit uint64) error {
	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	if err := a.accounts.prune(ctx, txFrom, txTo, limit, logEvery); err != nil {
		return err
	}
	if err := a.storage.prune(ctx, txFrom, txTo, limit, logEvery); err != nil {
		return err
	}
	if err := a.code.prune(ctx, txFrom, txTo, limit, logEvery); err != nil {
		return err
	}
	if err := a.logAddrs.prune(ctx, txFrom, txTo, limit, logEvery); err != nil {
		return err
	}
	if err := a.logTopics.prune(ctx, txFrom, txTo, limit, logEvery); err != nil {
		return err
	}
	if err := a.tracesFrom.prune(ctx, txFrom, txTo, limit, logEvery); err != nil {
		return err
	}
	if err := a.tracesTo.prune(ctx, txFrom, txTo, limit, logEvery); err != nil {
		return err
	}
	return nil
}

func (a *AggregatorV3) LogStats(tx kv.Tx, tx2block func(endTxNumMinimax uint64) uint64) {
	if a.maxTxNum.Load() == 0 {
		return
	}
	histBlockNumProgress := tx2block(a.maxTxNum.Load())
	str := make([]string, 0, a.accounts.InvertedIndex.files.Len())
	a.accounts.InvertedIndex.files.Ascend(func(it *filesItem) bool {
		bn := tx2block(it.endTxNum)
		str = append(str, fmt.Sprintf("%d=%dK", it.endTxNum/a.aggregationStep, bn/1_000))
		return true
	})

	c, err := tx.CursorDupSort(a.accounts.InvertedIndex.indexTable)
	if err != nil {
		// TODO pass error properly around
		panic(err)
	}
	_, v, err := c.First()
	if err != nil {
		// TODO pass error properly around
		panic(err)
	}
	var firstHistoryIndexBlockInDB uint64
	if len(v) != 0 {
		firstHistoryIndexBlockInDB = tx2block(binary.BigEndian.Uint64(v))
	}

	var m runtime.MemStats
	dbg.ReadMemStats(&m)
	log.Info("[Snapshots] History Stat",
		"blocks", fmt.Sprintf("%dk", (histBlockNumProgress+1)/1000),
		"txs", fmt.Sprintf("%dm", a.maxTxNum.Load()/1_000_000),
		"txNum2blockNum", strings.Join(str, ","),
		"first_history_idx_in_db", firstHistoryIndexBlockInDB,
		"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys))
}

func (a *AggregatorV3) EndTxNumMinimax() uint64 { return a.maxTxNum.Load() }
func (a *AggregatorV3) recalcMaxTxNum() {
	min := a.accounts.endTxNumMinimax()
	if txNum := a.storage.endTxNumMinimax(); txNum < min {
		min = txNum
	}
	if txNum := a.code.endTxNumMinimax(); txNum < min {
		min = txNum
	}
	if txNum := a.logAddrs.endTxNumMinimax(); txNum < min {
		min = txNum
	}
	if txNum := a.logTopics.endTxNumMinimax(); txNum < min {
		min = txNum
	}
	if txNum := a.tracesFrom.endTxNumMinimax(); txNum < min {
		min = txNum
	}
	if txNum := a.tracesTo.endTxNumMinimax(); txNum < min {
		min = txNum
	}
	a.maxTxNum.Store(min)
}

type RangesV3 struct {
	accounts             HistoryRanges
	storage              HistoryRanges
	code                 HistoryRanges
	logTopicsStartTxNum  uint64
	logAddrsEndTxNum     uint64
	logAddrsStartTxNum   uint64
	logTopicsEndTxNum    uint64
	tracesFromStartTxNum uint64
	tracesFromEndTxNum   uint64
	tracesToStartTxNum   uint64
	tracesToEndTxNum     uint64
	logAddrs             bool
	logTopics            bool
	tracesFrom           bool
	tracesTo             bool
}

func (r RangesV3) any() bool {
	return r.accounts.any() || r.storage.any() || r.code.any() || r.logAddrs || r.logTopics || r.tracesFrom || r.tracesTo
}

func (a *AggregatorV3) findMergeRange(maxEndTxNum, maxSpan uint64) RangesV3 {
	var r RangesV3
	r.accounts = a.accounts.findMergeRange(maxEndTxNum, maxSpan)
	r.storage = a.storage.findMergeRange(maxEndTxNum, maxSpan)
	r.code = a.code.findMergeRange(maxEndTxNum, maxSpan)
	r.logAddrs, r.logAddrsStartTxNum, r.logAddrsEndTxNum = a.logAddrs.findMergeRange(maxEndTxNum, maxSpan)
	r.logTopics, r.logTopicsStartTxNum, r.logTopicsEndTxNum = a.logTopics.findMergeRange(maxEndTxNum, maxSpan)
	r.tracesFrom, r.tracesFromStartTxNum, r.tracesFromEndTxNum = a.tracesFrom.findMergeRange(maxEndTxNum, maxSpan)
	r.tracesTo, r.tracesToStartTxNum, r.tracesToEndTxNum = a.tracesTo.findMergeRange(maxEndTxNum, maxSpan)
	//log.Info(fmt.Sprintf("findMergeRange(%d, %d)=%+v\n", maxEndTxNum, maxSpan, r))
	return r
}

type SelectedStaticFilesV3 struct {
	logTopics    []*filesItem
	accountsHist []*filesItem
	tracesTo     []*filesItem
	storageIdx   []*filesItem
	storageHist  []*filesItem
	tracesFrom   []*filesItem
	codeIdx      []*filesItem
	codeHist     []*filesItem
	accountsIdx  []*filesItem
	logAddrs     []*filesItem
	codeI        int
	logAddrsI    int
	logTopicsI   int
	storageI     int
	tracesFromI  int
	accountsI    int
	tracesToI    int
}

func (sf SelectedStaticFilesV3) Close() {
	for _, group := range [][]*filesItem{sf.accountsIdx, sf.accountsHist, sf.storageIdx, sf.accountsHist, sf.codeIdx, sf.codeHist,
		sf.logAddrs, sf.logTopics, sf.tracesFrom, sf.tracesTo} {
		for _, item := range group {
			if item != nil {
				if item.decompressor != nil {
					item.decompressor.Close()
				}
				if item.index != nil {
					item.index.Close()
				}
			}
		}
	}
}

func (a *AggregatorV3) staticFilesInRange(r RangesV3) SelectedStaticFilesV3 {
	var sf SelectedStaticFilesV3
	if r.accounts.any() {
		sf.accountsIdx, sf.accountsHist, sf.accountsI = a.accounts.staticFilesInRange(r.accounts)
	}
	if r.storage.any() {
		sf.storageIdx, sf.storageHist, sf.storageI = a.storage.staticFilesInRange(r.storage)
	}
	if r.code.any() {
		sf.codeIdx, sf.codeHist, sf.codeI = a.code.staticFilesInRange(r.code)
	}
	if r.logAddrs {
		sf.logAddrs, sf.logAddrsI = a.logAddrs.staticFilesInRange(r.logAddrsStartTxNum, r.logAddrsEndTxNum)
	}
	if r.logTopics {
		sf.logTopics, sf.logTopicsI = a.logTopics.staticFilesInRange(r.logTopicsStartTxNum, r.logTopicsEndTxNum)
	}
	if r.tracesFrom {
		sf.tracesFrom, sf.tracesFromI = a.tracesFrom.staticFilesInRange(r.tracesFromStartTxNum, r.tracesFromEndTxNum)
	}
	if r.tracesTo {
		sf.tracesTo, sf.tracesToI = a.tracesTo.staticFilesInRange(r.tracesToStartTxNum, r.tracesToEndTxNum)
	}
	return sf
}

type MergedFilesV3 struct {
	accountsIdx, accountsHist *filesItem
	storageIdx, storageHist   *filesItem
	codeIdx, codeHist         *filesItem
	logAddrs                  *filesItem
	logTopics                 *filesItem
	tracesFrom                *filesItem
	tracesTo                  *filesItem
}

func (mf MergedFilesV3) Close() {
	for _, item := range []*filesItem{mf.accountsIdx, mf.accountsHist, mf.storageIdx, mf.storageHist, mf.codeIdx, mf.codeHist,
		mf.logAddrs, mf.logTopics, mf.tracesFrom, mf.tracesTo} {
		if item != nil {
			if item.decompressor != nil {
				item.decompressor.Close()
			}
			if item.index != nil {
				item.index.Close()
			}
		}
	}
}

func (a *AggregatorV3) mergeFiles(ctx context.Context, files SelectedStaticFilesV3, r RangesV3, maxSpan uint64, workers int) (MergedFilesV3, error) {
	var mf MergedFilesV3
	closeFiles := true
	defer func() {
		if closeFiles {
			mf.Close()
		}
	}()
	//var wg sync.WaitGroup
	//wg.Add(7)
	errCh := make(chan error, 7)
	//go func() {
	//	defer wg.Done()
	var err error
	if r.accounts.any() {
		if mf.accountsIdx, mf.accountsHist, err = a.accounts.mergeFiles(ctx, files.accountsIdx, files.accountsHist, r.accounts, workers); err != nil {
			errCh <- err
		}
	}
	//}()
	//go func() {
	//	defer wg.Done()
	//	var err error
	if r.storage.any() {
		if mf.storageIdx, mf.storageHist, err = a.storage.mergeFiles(ctx, files.storageIdx, files.storageHist, r.storage, workers); err != nil {
			errCh <- err
		}
	}
	//}()
	//go func() {
	//	defer wg.Done()
	//	var err error
	if r.code.any() {
		if mf.codeIdx, mf.codeHist, err = a.code.mergeFiles(ctx, files.codeIdx, files.codeHist, r.code, workers); err != nil {
			errCh <- err
		}
	}
	//}()
	//go func() {
	//	defer wg.Done()
	//	var err error
	if r.logAddrs {
		if mf.logAddrs, err = a.logAddrs.mergeFiles(ctx, files.logAddrs, r.logAddrsStartTxNum, r.logAddrsEndTxNum, workers); err != nil {
			errCh <- err
		}
	}
	//}()
	//go func() {
	//	defer wg.Done()
	//	var err error
	if r.logTopics {
		if mf.logTopics, err = a.logTopics.mergeFiles(ctx, files.logTopics, r.logTopicsStartTxNum, r.logTopicsEndTxNum, workers); err != nil {
			errCh <- err
		}
	}
	//}()
	//go func() {
	//	defer wg.Done()
	//	var err error
	if r.tracesFrom {
		if mf.tracesFrom, err = a.tracesFrom.mergeFiles(ctx, files.tracesFrom, r.tracesFromStartTxNum, r.tracesFromEndTxNum, workers); err != nil {
			errCh <- err
		}
	}
	//}()
	//go func() {
	//	defer wg.Done()
	//	var err error
	if r.tracesTo {
		if mf.tracesTo, err = a.tracesTo.mergeFiles(ctx, files.tracesTo, r.tracesToStartTxNum, r.tracesToEndTxNum, workers); err != nil {
			errCh <- err
		}
	}
	//}()
	//go func() {
	//	wg.Wait()
	close(errCh)
	//}()
	var lastError error
	for err := range errCh {
		lastError = err
	}
	if lastError == nil {
		closeFiles = false
	}
	return mf, lastError
}

func (a *AggregatorV3) integrateMergedFiles(outs SelectedStaticFilesV3, in MergedFilesV3) {
	a.accounts.integrateMergedFiles(outs.accountsIdx, outs.accountsHist, in.accountsIdx, in.accountsHist)
	a.storage.integrateMergedFiles(outs.storageIdx, outs.storageHist, in.storageIdx, in.storageHist)
	a.code.integrateMergedFiles(outs.codeIdx, outs.codeHist, in.codeIdx, in.codeHist)
	a.logAddrs.integrateMergedFiles(outs.logAddrs, in.logAddrs)
	a.logTopics.integrateMergedFiles(outs.logTopics, in.logTopics)
	a.tracesFrom.integrateMergedFiles(outs.tracesFrom, in.tracesFrom)
	a.tracesTo.integrateMergedFiles(outs.tracesTo, in.tracesTo)
}

func (a *AggregatorV3) deleteFiles(outs SelectedStaticFilesV3) error {
	if err := a.accounts.deleteFiles(outs.accountsIdx, outs.accountsHist); err != nil {
		return err
	}
	if err := a.storage.deleteFiles(outs.storageIdx, outs.storageHist); err != nil {
		return err
	}
	if err := a.code.deleteFiles(outs.codeIdx, outs.codeHist); err != nil {
		return err
	}
	if err := a.logAddrs.deleteFiles(outs.logAddrs); err != nil {
		return err
	}
	if err := a.logTopics.deleteFiles(outs.logTopics); err != nil {
		return err
	}
	if err := a.tracesFrom.deleteFiles(outs.tracesFrom); err != nil {
		return err
	}
	if err := a.tracesTo.deleteFiles(outs.tracesTo); err != nil {
		return err
	}
	return nil
}

// KeepInDB - usually equal to one a.aggregationStep, but when we exec blocks from snapshots
// we can set it to 0, because no re-org on this blocks are possible
func (a *AggregatorV3) KeepInDB(v uint64) { a.keepInDB = v }

func (a *AggregatorV3) BuildFilesInBackground(db kv.RoDB) error {
	if (a.txNum.Load() + 1) <= a.maxTxNum.Load()+a.aggregationStep+a.keepInDB { // Leave one step worth in the DB
		return nil
	}

	step := a.maxTxNum.Load() / a.aggregationStep
	if a.working.Load() {
		return nil
	}

	toTxNum := (step + 1) * a.aggregationStep
	hasData := false

	a.working.Store(true)
	go func() {
		defer a.working.Store(false)

		// check if db has enough data (maybe we didn't commit them yet)
		lastInDB := lastIdInDB(db, a.accounts.indexKeysTable)
		hasData = lastInDB >= toTxNum
		if !hasData {
			return
		}

		// trying to create as much small-step-files as possible:
		// - to reduce amount of small merges
		// - to remove old data from db as early as possible
		// - during files build, may happen commit of new data. on each loop step getting latest id in db
		for step < lastIdInDB(db, a.accounts.indexKeysTable)/a.aggregationStep {
			if err := a.buildFilesInBackground(a.ctx, step, db); err != nil {
				log.Warn("buildFilesInBackground", "err", err)
				break
			}
			step++
		}

		if a.workingMerge.Load() {
			return
		}
		a.workingMerge.Store(true)
		go func() {
			defer a.workingMerge.Store(false)
			if err := a.MergeLoop(a.ctx, 1); err != nil {
				log.Warn("merge", "err", err)
			}

			a.BuildOptionalMissedIndices(a.ctx)
		}()
	}()

	//if err := a.prune(0, a.maxTxNum.Load(), a.aggregationStep); err != nil {
	//	return err
	//}
	return nil
}

func (a *AggregatorV3) AddAccountPrev(addr []byte, prev []byte) error {
	if err := a.accounts.AddPrevValue(addr, nil, prev); err != nil {
		return err
	}
	return nil
}

func (a *AggregatorV3) AddStoragePrev(addr []byte, loc []byte, prev []byte) error {
	if err := a.storage.AddPrevValue(addr, loc, prev); err != nil {
		return err
	}
	return nil
}

// AddCodePrev - addr+inc => code
func (a *AggregatorV3) AddCodePrev(addr []byte, prev []byte) error {
	if err := a.code.AddPrevValue(addr, nil, prev); err != nil {
		return err
	}
	return nil
}

func (a *AggregatorV3) AddTraceFrom(addr []byte) error {
	return a.tracesFrom.Add(addr)
}

func (a *AggregatorV3) AddTraceTo(addr []byte) error {
	return a.tracesTo.Add(addr)
}

func (a *AggregatorV3) AddLogAddr(addr []byte) error {
	return a.logAddrs.Add(addr)
}

func (a *AggregatorV3) AddLogTopic(topic []byte) error {
	return a.logTopics.Add(topic)
}

// DisableReadAhead - usage: `defer d.EnableReadAhead().DisableReadAhead()`. Please don't use this funcs without `defer` to avoid leak.
func (a *AggregatorV3) DisableReadAhead() {
	a.accounts.DisableReadAhead()
	a.storage.DisableReadAhead()
	a.code.DisableReadAhead()
	a.logAddrs.DisableReadAhead()
	a.logTopics.DisableReadAhead()
	a.tracesFrom.DisableReadAhead()
	a.tracesTo.DisableReadAhead()
}
func (a *AggregatorV3) EnableReadAhead() *AggregatorV3 {
	a.accounts.EnableReadAhead()
	a.storage.EnableReadAhead()
	a.code.EnableReadAhead()
	a.logAddrs.EnableReadAhead()
	a.logTopics.EnableReadAhead()
	a.tracesFrom.EnableReadAhead()
	a.tracesTo.EnableReadAhead()
	return a
}
func (a *AggregatorV3) EnableMadvWillNeed() *AggregatorV3 {
	a.accounts.EnableMadvWillNeed()
	a.storage.EnableMadvWillNeed()
	a.code.EnableMadvWillNeed()
	a.logAddrs.EnableMadvWillNeed()
	a.logTopics.EnableMadvWillNeed()
	a.tracesFrom.EnableMadvWillNeed()
	a.tracesTo.EnableMadvWillNeed()
	return a
}
func (a *AggregatorV3) EnableMadvNormal() *AggregatorV3 {
	a.accounts.EnableMadvNormalReadAhead()
	a.storage.EnableMadvNormalReadAhead()
	a.code.EnableMadvNormalReadAhead()
	a.logAddrs.EnableMadvNormalReadAhead()
	a.logTopics.EnableMadvNormalReadAhead()
	a.tracesFrom.EnableMadvNormalReadAhead()
	a.tracesTo.EnableMadvNormalReadAhead()
	return a
}

// -- range
func (ac *AggregatorV3Context) LogAddrIterator(addr []byte, startTxNum, endTxNum int, asc order.By, limit int, roTx kv.Tx) (*InvertedIterator, error) {
	return ac.logAddrs.IterateRange(addr, startTxNum, endTxNum, asc, limit, roTx)
}

func (ac *AggregatorV3Context) LogTopicIterator(topic []byte, startTxNum, endTxNum int, asc order.By, limit int, roTx kv.Tx) (*InvertedIterator, error) {
	return ac.logTopics.IterateRange(topic, startTxNum, endTxNum, asc, limit, roTx)
}

func (ac *AggregatorV3Context) TraceFromIterator(addr []byte, startTxNum, endTxNum int, asc order.By, limit int, roTx kv.Tx) (*InvertedIterator, error) {
	return ac.tracesFrom.IterateRange(addr, startTxNum, endTxNum, asc, limit, roTx)
}

func (ac *AggregatorV3Context) TraceToIterator(addr []byte, startTxNum, endTxNum int, asc order.By, limit int, roTx kv.Tx) (*InvertedIterator, error) {
	return ac.tracesTo.IterateRange(addr, startTxNum, endTxNum, asc, limit, roTx)
}
func (ac *AggregatorV3Context) AccountHistoyIdxIterator(addr []byte, startTxNum, endTxNum int, asc order.By, limit int, roTx kv.Tx) (*InvertedIterator, error) {
	//TODO: don't create new context by MakeContext
	return ac.accounts.h.InvertedIndex.MakeContext().IterateRange(addr, startTxNum, endTxNum, asc, limit, roTx)
}
func (ac *AggregatorV3Context) StorageHistoyIdxIterator(addr []byte, startTxNum, endTxNum int, asc order.By, limit int, roTx kv.Tx) (*InvertedIterator, error) {
	//TODO: don't create new context by MakeContext
	return ac.storage.h.InvertedIndex.MakeContext().IterateRange(addr, startTxNum, endTxNum, asc, limit, roTx)
}
func (ac *AggregatorV3Context) CodeHistoyIdxIterator(addr []byte, startTxNum, endTxNum int, asc order.By, limit int, roTx kv.Tx) (*InvertedIterator, error) {
	//TODO: don't create new context by MakeContext
	return ac.code.h.InvertedIndex.MakeContext().IterateRange(addr, startTxNum, endTxNum, asc, limit, roTx)
}

// -- range end

func (ac *AggregatorV3Context) ReadAccountDataNoStateWithRecent(addr []byte, txNum uint64) ([]byte, bool, error) {
	return ac.accounts.GetNoStateWithRecent(addr, txNum, ac.tx)
}

func (ac *AggregatorV3Context) ReadAccountDataNoState(addr []byte, txNum uint64) ([]byte, bool, error) {
	return ac.accounts.GetNoState(addr, txNum)
}

func (ac *AggregatorV3Context) ReadAccountStorageNoStateWithRecent(addr []byte, loc []byte, txNum uint64) ([]byte, bool, error) {
	if cap(ac.keyBuf) < len(addr)+len(loc) {
		ac.keyBuf = make([]byte, len(addr)+len(loc))
	} else if len(ac.keyBuf) != len(addr)+len(loc) {
		ac.keyBuf = ac.keyBuf[:len(addr)+len(loc)]
	}
	copy(ac.keyBuf, addr)
	copy(ac.keyBuf[len(addr):], loc)
	return ac.storage.GetNoStateWithRecent(ac.keyBuf, txNum, ac.tx)
}
func (ac *AggregatorV3Context) ReadAccountStorageNoStateWithRecent2(key []byte, txNum uint64) ([]byte, bool, error) {
	return ac.storage.GetNoStateWithRecent(key, txNum, ac.tx)
}

func (ac *AggregatorV3Context) ReadAccountStorageNoState(addr []byte, loc []byte, txNum uint64) ([]byte, bool, error) {
	if cap(ac.keyBuf) < len(addr)+len(loc) {
		ac.keyBuf = make([]byte, len(addr)+len(loc))
	} else if len(ac.keyBuf) != len(addr)+len(loc) {
		ac.keyBuf = ac.keyBuf[:len(addr)+len(loc)]
	}
	copy(ac.keyBuf, addr)
	copy(ac.keyBuf[len(addr):], loc)
	return ac.storage.GetNoState(ac.keyBuf, txNum)
}

func (ac *AggregatorV3Context) ReadAccountCodeNoStateWithRecent(addr []byte, txNum uint64) ([]byte, bool, error) {
	return ac.code.GetNoStateWithRecent(addr, txNum, ac.tx)
}
func (ac *AggregatorV3Context) ReadAccountCodeNoState(addr []byte, txNum uint64) ([]byte, bool, error) {
	return ac.code.GetNoState(addr, txNum)
}

func (ac *AggregatorV3Context) ReadAccountCodeSizeNoStateWithRecent(addr []byte, txNum uint64) (int, bool, error) {
	code, noState, err := ac.code.GetNoStateWithRecent(addr, txNum, ac.tx)
	if err != nil {
		return 0, false, err
	}
	return len(code), noState, nil
}
func (ac *AggregatorV3Context) ReadAccountCodeSizeNoState(addr []byte, txNum uint64) (int, bool, error) {
	code, noState, err := ac.code.GetNoState(addr, txNum)
	if err != nil {
		return 0, false, err
	}
	return len(code), noState, nil
}

func (ac *AggregatorV3Context) AccountHistoryIterateChanged(startTxNum, endTxNum uint64, roTx kv.Tx) *HistoryIterator1 {
	return ac.accounts.IterateChanged(startTxNum, endTxNum, roTx)
}

func (ac *AggregatorV3Context) StorageHistoryIterateChanged(startTxNum, endTxNum uint64, roTx kv.Tx) *HistoryIterator1 {
	return ac.storage.IterateChanged(startTxNum, endTxNum, roTx)
}

func (ac *AggregatorV3Context) AccountHistoricalStateRange(startTxNum uint64, from, to []byte, amount int, roTx kv.Tx) *WalkAsOfIter {
	return ac.accounts.WalkAsOf(startTxNum, from, to, roTx, amount)
}

func (ac *AggregatorV3Context) StorageHistoricalStateRange(startTxNum uint64, from, to []byte, amount int, roTx kv.Tx) *WalkAsOfIter {
	return ac.storage.WalkAsOf(startTxNum, from, to, roTx, amount)
}

func (ac *AggregatorV3Context) CodeHistoricalStateRange(startTxNum uint64, from, to []byte, amount int, roTx kv.Tx) *WalkAsOfIter {
	return ac.code.WalkAsOf(startTxNum, from, to, roTx, amount)
}

type FilesStats22 struct {
}

func (a *AggregatorV3) Stats() FilesStats22 {
	var fs FilesStats22
	return fs
}

func (a *AggregatorV3) Code() *History     { return a.code }
func (a *AggregatorV3) Accounts() *History { return a.accounts }
func (a *AggregatorV3) Storage() *History  { return a.storage }

type AggregatorV3Context struct {
	tx         kv.Tx
	a          *AggregatorV3
	accounts   *HistoryContext
	storage    *HistoryContext
	code       *HistoryContext
	logAddrs   *InvertedIndexContext
	logTopics  *InvertedIndexContext
	tracesFrom *InvertedIndexContext
	tracesTo   *InvertedIndexContext
	keyBuf     []byte
}

func (a *AggregatorV3) MakeContext() *AggregatorV3Context {
	return &AggregatorV3Context{
		a:          a,
		accounts:   a.accounts.MakeContext(),
		storage:    a.storage.MakeContext(),
		code:       a.code.MakeContext(),
		logAddrs:   a.logAddrs.MakeContext(),
		logTopics:  a.logTopics.MakeContext(),
		tracesFrom: a.tracesFrom.MakeContext(),
		tracesTo:   a.tracesTo.MakeContext(),
	}
}
func (ac *AggregatorV3Context) SetTx(tx kv.Tx) { ac.tx = tx }
func (ac *AggregatorV3Context) Close()         {}

// BackgroundResult - used only indicate that some work is done
// no much reason to pass exact results by this object, just get latest state when need
type BackgroundResult struct {
	err error
	has bool
}

func (br *BackgroundResult) Has() bool     { return br.has }
func (br *BackgroundResult) Set(err error) { br.has, br.err = true, err }
func (br *BackgroundResult) GetAndReset() (bool, error) {
	has, err := br.has, br.err
	br.has, br.err = false, nil
	return has, err
}

func lastIdInDB(db kv.RoDB, table string) (lstInDb uint64) {
	if err := db.View(context.Background(), func(tx kv.Tx) error {
		lst, _ := kv.LastKey(tx, table)
		if len(lst) > 0 {
			lstInDb = binary.BigEndian.Uint64(lst)
		}
		return nil
	}); err != nil {
		log.Warn("lastIdInDB", "err", err)
	}
	return lstInDb
}

// AggregatorStep is used for incremental reconstitution, it allows
// accessing history in isolated way for each step
type AggregatorStep struct {
	a        *AggregatorV3
	accounts *HistoryStep
	storage  *HistoryStep
	code     *HistoryStep
	keyBuf   []byte
}

func (a *AggregatorV3) MakeSteps() ([]*AggregatorStep, error) {
	to := a.maxTxNum.Load()
	indexedMax := cmp.Min(
		cmp.Min(a.accounts.endIndexedTxNumMinimax(), a.storage.endIndexedTxNumMinimax()),
		a.code.endIndexedTxNumMinimax(),
	)
	if to != indexedMax {
		log.Warn("[snapshots] not all files are indexed", "files", to/a.aggregationStep, "indexed", indexedMax/a.aggregationStep)
		to = cmp.Min(to, indexedMax)
	}
	accountSteps := a.accounts.MakeSteps(to)
	codeSteps := a.code.MakeSteps(to)
	storageSteps := a.storage.MakeSteps(to)
	if len(accountSteps) != len(storageSteps) || len(storageSteps) != len(codeSteps) {
		return nil, fmt.Errorf("different limit of steps (try merge snapshots): accountSteps=%d, storageSteps=%d, codeSteps=%d", len(accountSteps), len(storageSteps), len(codeSteps))
	}
	steps := make([]*AggregatorStep, len(accountSteps))
	for i, accountStep := range accountSteps {
		steps[i] = &AggregatorStep{
			a:        a,
			accounts: accountStep,
			storage:  storageSteps[i],
			code:     codeSteps[i],
		}
	}
	return steps, nil
}

func (as *AggregatorStep) TxNumRange() (uint64, uint64) {
	return as.accounts.indexFile.startTxNum, as.accounts.indexFile.endTxNum
}

func (as *AggregatorStep) IterateAccountsTxs() *ScanIteratorInc {
	return as.accounts.iterateTxs()
}

func (as *AggregatorStep) IterateStorageTxs() *ScanIteratorInc {
	return as.storage.iterateTxs()
}

func (as *AggregatorStep) IterateCodeTxs() *ScanIteratorInc {
	return as.code.iterateTxs()
}

func (as *AggregatorStep) ReadAccountDataNoState(addr []byte, txNum uint64) ([]byte, bool, uint64) {
	return as.accounts.GetNoState(addr, txNum)
}

func (as *AggregatorStep) ReadAccountStorageNoState(addr []byte, loc []byte, txNum uint64) ([]byte, bool, uint64) {
	if cap(as.keyBuf) < len(addr)+len(loc) {
		as.keyBuf = make([]byte, len(addr)+len(loc))
	} else if len(as.keyBuf) != len(addr)+len(loc) {
		as.keyBuf = as.keyBuf[:len(addr)+len(loc)]
	}
	copy(as.keyBuf, addr)
	copy(as.keyBuf[len(addr):], loc)
	return as.storage.GetNoState(as.keyBuf, txNum)
}

func (as *AggregatorStep) ReadAccountCodeNoState(addr []byte, txNum uint64) ([]byte, bool, uint64) {
	return as.code.GetNoState(addr, txNum)
}

func (as *AggregatorStep) ReadAccountCodeSizeNoState(addr []byte, txNum uint64) (int, bool, uint64) {
	code, noState, stateTxNum := as.code.GetNoState(addr, txNum)
	return len(code), noState, stateTxNum
}

func (as *AggregatorStep) MaxTxNumAccounts(addr []byte) (bool, uint64) {
	return as.accounts.MaxTxNum(addr)
}

func (as *AggregatorStep) MaxTxNumStorage(addr []byte, loc []byte) (bool, uint64) {
	if cap(as.keyBuf) < len(addr)+len(loc) {
		as.keyBuf = make([]byte, len(addr)+len(loc))
	} else if len(as.keyBuf) != len(addr)+len(loc) {
		as.keyBuf = as.keyBuf[:len(addr)+len(loc)]
	}
	copy(as.keyBuf, addr)
	copy(as.keyBuf[len(addr):], loc)
	return as.storage.MaxTxNum(as.keyBuf)
}

func (as *AggregatorStep) MaxTxNumCode(addr []byte) (bool, uint64) {
	return as.code.MaxTxNum(addr)
}

func (as *AggregatorStep) IterateAccountsHistory(txNum uint64) *HistoryIteratorInc {
	return as.accounts.interateHistoryBeforeTxNum(txNum)
}

func (as *AggregatorStep) IterateStorageHistory(txNum uint64) *HistoryIteratorInc {
	return as.storage.interateHistoryBeforeTxNum(txNum)
}

func (as *AggregatorStep) IterateCodeHistory(txNum uint64) *HistoryIteratorInc {
	return as.code.interateHistoryBeforeTxNum(txNum)
}

func (as *AggregatorStep) Clone() *AggregatorStep {
	return &AggregatorStep{
		a:        as.a,
		accounts: as.accounts.Clone(),
		storage:  as.storage.Clone(),
		code:     as.code.Clone(),
	}
}
