package backend

import (
	"math/big"
	"sync"
	"time"

	"github.com/simplechain-org/go-simplechain/common"
	"github.com/simplechain-org/go-simplechain/event"
	"github.com/simplechain-org/go-simplechain/log"

	"github.com/simplechain-org/go-simplechain/cross"
	cc "github.com/simplechain-org/go-simplechain/cross/core"
	db "github.com/simplechain-org/go-simplechain/cross/database"
	cm "github.com/simplechain-org/go-simplechain/cross/metric"
	"github.com/simplechain-org/go-simplechain/cross/trigger"

	"github.com/asdine/storm/v3"
	"github.com/asdine/storm/v3/q"
	lru "github.com/hashicorp/golang-lru"
)

const (
	expireInterval    = time.Minute * 5
	expireQueueNumber = 63
)

type store interface {
	GetStore(chainID *big.Int) (db.CtxDB, error)
	Add(ctx *cc.CrossTransactionWithSignatures) error
	Get(chainID *big.Int, ctxID common.Hash) *cc.CrossTransactionWithSignatures
}

type finishedLog interface {
	IsFinish(cc.CtxID) bool
}

// CrossPool is used for collecting signatures
type CrossPool struct {
	chainID *big.Int
	config  *cross.Config

	store        store
	retriever    trigger.ChainRetriever
	pending      *db.CtxSortedByBlockNum //带有local签名
	queued       *db.CtxSortedByBlockNum //网络其他节点签名
	pendingCache *lru.Cache              // cache signed pending ctx

	commitFeed  event.Feed
	commitScope event.SubscriptionScope

	signer   cc.CtxSigner
	signHash cc.SignHash
	txLog    finishedLog

	mu     sync.RWMutex
	wg     sync.WaitGroup // for shutdown sync
	stopCh chan struct{}

	logger log.Logger
}

func NewCrossPool(chainID *big.Int, config *cross.Config, store store, txLog finishedLog,
	retriever trigger.ChainRetriever, signHash cc.SignHash) *CrossPool {

	pendingCache, _ := lru.New(signedPendingSize)
	logger := log.New("X-module", "pool")

	pool := &CrossPool{
		chainID:      chainID,
		config:       config,
		store:        store,
		txLog:        txLog,
		retriever:    retriever,
		pending:      db.NewCtxSortedMap(),
		queued:       db.NewCtxSortedMap(),
		pendingCache: pendingCache,
		signer:       cc.MakeCtxSigner(chainID),
		signHash:     signHash,
		stopCh:       make(chan struct{}),
		logger:       logger,
	}

	if err := pool.load(); err != nil {
		logger.Error("Load pending transaction failed", "error", err)
	}

	return pool
}

func (pool *CrossPool) load() error {
	store, err := pool.store.GetStore(pool.chainID)
	if err != nil {
		return err
	}
	pending := store.Query(0, 0, []db.FieldName{db.BlockNumField}, false, q.Eq(db.StatusField, uint8(cc.CtxStatusPending)))
	for _, pendingTX := range pending {
		pool.pending.Put(pendingTX)
	}
	return nil
}

func (pool *CrossPool) loop() {
	defer pool.wg.Done()
	expire := time.NewTicker(expireInterval)
	defer expire.Stop()

	for {
		select {
		case <-pool.stopCh:
			return

		case <-expire.C:
			currentNum, expireNum := pool.retriever.CurrentBlockNumber(), pool.retriever.ExpireNumber()
			pool.queued.RemoveUnderNum(pool.retriever.CurrentBlockNumber() - expireQueueNumber)
			if expireNum < 0 { //never expired, only clean queued regularly
				break
			}
			var removed cc.CtxIDs
			if currentNum > uint64(expireNum) {
				removed = append(removed, pool.pending.RemoveUnderNum(currentNum-uint64(expireNum))...)
				removed = append(removed, pool.queued.RemoveUnderNum(currentNum-uint64(expireNum))...)
			}
			if len(removed) > 0 {
				cm.Report(pool.chainID.Uint64(), "txs expired", "ids", removed.String())
			}
		}
	}
}

func (pool *CrossPool) Stop() {
	pool.commitScope.Close()
	close(pool.stopCh)
	pool.wg.Wait()
}

// AddLocal CrossTransactions synced from blockchain subscriber
func (pool *CrossPool) AddLocals(txs ...*cc.CrossTransaction) (signed []*cc.CrossTransaction, errs []error) {
	for _, ctx := range txs {
		if pool.txLog.IsFinish(ctx.ID()) {
			// already exist in finished log
			errs = append(errs, cross.ErrFinishedCtx)
			continue
		}
		if err := pool.verifyReorg(ctx); err != nil {
			errs = append(errs, err)
			continue
		}
		if pool.store.Get(pool.chainID, ctx.ID()) != nil {
			// already exist in store, ignore
			errs = append(errs, cross.ErrAlreadyExistCtx)
			continue
		}
		// make signature first for local ctx
		signedTx, err := pool.signTx(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		signed = append(signed, signedTx)
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, signedTx := range signed {
		if err := pool.addTx(signedTx, true); err != nil {
			errs = append(errs, err)
		}
	}
	return signed, errs
}

// GetLocal get local signed CrossTransaction from pool & store
func (pool *CrossPool) GetLocal(ctxID common.Hash) *cc.CrossTransaction {
	// find in cache
	if ctx, ok := pool.pendingCache.Get(ctxID); ok {
		return ctx.(*cc.CrossTransaction)
	}
	// find in pending
	if cws := pool.pending.Get(ctxID); cws != nil {
		if ctx, _ := pool.signTx(cws.CrossTransaction()); ctx != nil {
			return ctx
		}
	}
	// find in localStore
	if cws := pool.store.Get(pool.chainID, ctxID); cws != nil {
		if ctx, _ := pool.signTx(cws.CrossTransaction()); ctx != nil {
			return ctx
		}
	}
	return nil
}

// AddRemote CrossTransactions received from peers
func (pool *CrossPool) AddRemote(ctx *cc.CrossTransaction) (signer common.Address, err error) {
	signer, err = pool.retriever.VerifySigner(ctx, ctx.ChainId(), ctx.DestinationId())
	if err != nil {
		return signer, err
	}
	// self signer ignore
	if signer == pool.config.Signer {
		return signer, nil
	}
	if pool.txLog.IsFinish(ctx.ID()) {
		// already exist in finished log
		return signer, cross.ErrFinishedCtx
	}
	if old := pool.store.Get(pool.chainID, ctx.ID()); old != nil && old.Status != cc.CtxStatusPending {
		pool.logger.Debug("ctx is already signed", "ctxID", ctx.ID().String())
		return signer, cross.ErrAlreadyExistCtx
	}
	if err := pool.retriever.VerifyExpire(ctx); err != nil {
		return signer, err
	}
	if err := pool.retriever.VerifyContract(ctx); err != nil {
		return signer, err
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	return signer, pool.addTx(ctx, false)
}

func (pool *CrossPool) signTx(ctx *cc.CrossTransaction) (*cc.CrossTransaction, error) {
	ctx, err := cc.SignCtx(ctx, pool.signer, pool.signHash)
	if err != nil {
		return nil, err
	}
	pool.pendingCache.Add(ctx.ID(), ctx) // add to cache
	return ctx, nil
}

func (pool *CrossPool) addTx(ctx *cc.CrossTransaction, local bool) error {
	id := ctx.ID()

	checkAndCommit := func(id common.Hash) error {
		if cws := pool.pending.Get(id); cws != nil && cws.SignaturesLength() >= pool.retriever.RequireSignatures() {
			pool.Commit(cws)
		}
		return nil
	}

	// if this pending ctx exist, add signature to pending directly
	if cws := pool.pending.Get(id); cws != nil {
		if err := cws.AddSignature(ctx); err != nil {
			return err
		}
		return checkAndCommit(id)
	}

	// add new local ctx, move queued signatures of this ctx to pendin g
	if local {
		pendingRws := cc.NewCrossTransactionWithSignatures(ctx, pool.retriever.GetConfirmedTransactionNumberOnChain(ctx))
		// promote queued ctx to pending, update to number received by local
		// move cws from queued to pending
		if queuedRws := pool.queued.Get(id); queuedRws != nil {
			if err := queuedRws.AddSignature(ctx); err != nil {
				return err
			}
			pendingRws = queuedRws
		}
		pool.pending.Put(pendingRws)
		pool.queued.RemoveByID(id)
		return checkAndCommit(id)
	}

	// add new remote ctx, only add to pending pool
	if cws := pool.queued.Get(id); cws != nil {
		if err := cws.AddSignature(ctx); err != nil {
			return err
		}
	} else {
		pool.queued.Put(cc.NewCrossTransactionWithSignatures(ctx, pool.retriever.GetConfirmedTransactionNumberOnChain(ctx)))
	}
	return nil
}

// verifyReorg compares blockHash to verify blockchain reorg
func (pool *CrossPool) verifyReorg(ctx *cc.CrossTransaction) error {
	if old := pool.store.Get(pool.chainID, ctx.ID()); old != nil {
		if ctx.BlockHash() != old.BlockHash() {
			pool.logger.Warn("blockchain reorg,txId:%s,old:%s,new:%s", ctx.ID().String(), old.BlockHash().String(), ctx.BlockHash().String())
			cm.Report(pool.chainID.Uint64(), "blockchain reorg", "ctxID", ctx.ID().String(),
				"old", old.BlockHash().String(), "new", ctx.BlockHash().String())
			return cross.ErrReorgCtx
		}
	}
	return nil
}

// Commit signed ctx with callback
func (pool *CrossPool) Commit(cws *cc.CrossTransactionWithSignatures) {
	pool.pending.RemoveByID(cws.ID()) // remove it from pending
	pool.wg.Add(1)
	go func() { //TODO: 同步还是异步？
		defer pool.wg.Done()
		pool.commitFeed.Send(cc.SignedCtxEvent{
			Tx: cws,
			CallBack: func(cws *cc.CrossTransactionWithSignatures, invalidSigIndex ...int) {
				if invalidSigIndex == nil { // check signer successfully, store ctx
					pool.Store(cws)
				} else { // check failed, rollback
					pool.Rollback(cws, invalidSigIndex)
				}
			}})
	}()
}

// Store ctx into CrossStore
func (pool *CrossPool) Store(cws *cc.CrossTransactionWithSignatures) {
	cws.SetStatus(cc.CtxStatusWaiting)
	// if pending exist, update to waiting
	if err := pool.store.Add(cws); err != nil && err != storm.ErrAlreadyExists {
		pool.logger.Warn("Store local ctx failed", "txID", cws.ID(), "err", err)
	}
}

// Rollback ctx to pending and remove its invalid signatures
func (pool *CrossPool) Rollback(cws *cc.CrossTransactionWithSignatures, invalidSigIndex []int) {
	pool.logger.Warn("pending rollback for invalid signature", "ctxID", cws.ID(), "invalidSigIndex", invalidSigIndex)
	for _, invalid := range invalidSigIndex {
		cws.RemoveSignature(invalid)
	}
	pool.pending.Put(cws)
	cm.Report(pool.chainID.Uint64(), "pending rollback for invalid signature", "ctxID", cws.ID(), "invalidSigIndex", invalidSigIndex)
}

// report pending and queue's length
func (pool *CrossPool) Stats() (int, int) {
	return pool.pending.Len(), pool.queued.Len()
}

// Pending return pending ctx by height
func (pool *CrossPool) Pending(startNumber uint64, limit int) (ids []common.Hash, pending []*cc.CrossTransactionWithSignatures) {
	pool.pending.Map(func(ctx *cc.CrossTransactionWithSignatures) bool {
		if ctx.BlockNum <= startNumber { // 低于起始高度的pending不取
			return false
		}
		if pending != nil && len(pending) >= limit && pending[len(pending)-1].BlockNum != ctx.BlockNum {
			return true
		}
		ids = append(ids, ctx.ID())
		pending = append(pending, ctx)
		return false
	})
	return ids, pending
}

func (pool *CrossPool) SubscribeSignedCtxEvent(ch chan<- cc.SignedCtxEvent) event.Subscription {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.commitScope.Track(pool.commitFeed.Subscribe(ch))
}
