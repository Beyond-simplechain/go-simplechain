package backend

import (
	"math/big"
	"sort"
	"sync/atomic"
	"time"

	"github.com/simplechain-org/go-simplechain/common"
	"github.com/simplechain-org/go-simplechain/cross/core"
	"github.com/simplechain-org/go-simplechain/log"
)

type SyncReq struct {
	Chain  uint64
	Height uint64
}

type SyncResp struct {
	Chain uint64
	Data  [][]byte
}

type SyncPendingReq struct {
	Chain uint64
	Ids   []common.Hash
}

type SyncPendingResp struct {
	Chain uint64
	Data  [][]byte
}

type SortedTxByBlockNum []*core.CrossTransactionWithSignatures

func (s SortedTxByBlockNum) Len() int           { return len(s) }
func (s SortedTxByBlockNum) Less(i, j int) bool { return s[i].BlockNum < s[j].BlockNum }
func (s SortedTxByBlockNum) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

func (srv *CrossService) sync() {
	ticker := time.NewTicker(time.Second * 30)
	for {
		select {
		case p := <-srv.newPeerCh:
			if srv.peers.Len() > 0 {
				srv.synchronise(srv.peers.BestPeer())
			}

			peers := map[string]*anchorPeer{p.id: p}
			go srv.syncPending(srv.main.handler, peers)
			go srv.syncPending(srv.sub.handler, peers)

		case <-ticker.C:
			if srv.peers.Len() == 0 {
				break
			}
			go srv.syncPending(srv.main.handler, srv.peers.peers)
			go srv.syncPending(srv.sub.handler, srv.peers.peers)

		case <-srv.quitSync:
			return
		}
	}
}

func (srv *CrossService) synchronise(main, sub *anchorPeer) {
	go srv.syncWithPeer(srv.main.handler, main, main.crossStatus.MainHeight)
	go srv.syncWithPeer(srv.sub.handler, sub, sub.crossStatus.SubHeight)
}

func (srv *CrossService) syncWithPeer(handler *Handler, peer *anchorPeer, height *big.Int) {
	if !atomic.CompareAndSwapUint32(&handler.synchronising, 0, 1) {
		peer.Log().Debug("sync busy")
		return
	}

	// ignore prev sync
	for empty := false; !empty; {
		select {
		case <-handler.synchronizeCh:
		default:
			empty = true
		}
	}

	if h := handler.Height(); h.Cmp(height) <= 0 {
		go peer.RequestCtxSyncByHeight(handler.pm.NetworkId(), h.Uint64())
	}

	timeout := time.NewTimer(rttMaxEstimate)
	defer timeout.Stop()
	for {
		select {
		case txs := <-handler.synchronizeCh:
			if len(txs) == 0 {
				atomic.StoreUint32(&handler.synchronising, 0)
				peer.Log().Debug("sync ctx request completed")
				return
			}
			var mainTxs, subTxs []*core.CrossTransactionWithSignatures
			for _, tx := range txs {
				if tx.ChainId().Uint64() == srv.main.chainID {
					mainTxs = append(mainTxs, tx)
				}
				if tx.ChainId().Uint64() == srv.sub.chainID {
					subTxs = append(subTxs, tx)
				}
			}
			var main, sub int
			if mainTxs != nil {
				sort.Sort(SortedTxByBlockNum(mainTxs))
				main = srv.main.handler.SyncCrossTransaction(mainTxs)
			}
			if subTxs != nil {
				sort.Sort(SortedTxByBlockNum(subTxs))
				sub = srv.sub.handler.SyncCrossTransaction(subTxs)
			}

			log.Info("Import cross transactions", "total", len(txs), "main", main, "sub", sub)

			timeout.Reset(rttMaxEstimate)
			// send next sync request after last
			go peer.RequestCtxSyncByHeight(handler.pm.NetworkId(), txs[len(txs)-1].BlockNum+1)

		case <-timeout.C:
			atomic.StoreUint32(&handler.synchronising, 0)
			peer.Log().Debug("sync ctx request timed out")
			return
		}
	}
}

func (srv *CrossService) syncPending(handler *Handler, peers map[string]*anchorPeer) {
	pending := handler.Pending(0, defaultMaxSyncSize)
	peerWithPending := make(map[string][]common.Hash)
	for _, id := range pending {
		for pid, p := range peers {
			if !p.knownCTxs.Contains(id) {
				peerWithPending[pid] = append(peerWithPending[pid], id)
			}
		}
	}
	for pid, pending := range peerWithPending {
		peer := srv.peers.Peer(pid)
		srv.syncPendingWithPeer(handler, peer, pending)
	}
}

func (srv *CrossService) syncPendingWithPeer(handler *Handler, p *anchorPeer, pending []common.Hash) {
	if p == nil || pending == nil {
		return
	}
	go p.SendSyncPendingRequest(handler.pm.NetworkId(), pending)
	p.Log().Info("start sync pending cross transaction", "chainID", handler.pm.NetworkId(), "pending", len(pending), "peer", p.id)
}
