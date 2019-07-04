// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package miner implements Ethereum block creation and mining.
package miner

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/consensus"
	"github.com/ledgerwatch/turbo-geth/core"
	"github.com/ledgerwatch/turbo-geth/core/state"
	"github.com/ledgerwatch/turbo-geth/core/types"
	"github.com/ledgerwatch/turbo-geth/eth/downloader"
	"github.com/ledgerwatch/turbo-geth/event"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/ledgerwatch/turbo-geth/params"
)

// Backend wraps all methods required for mining.
type Backend interface {
	BlockChain() *core.BlockChain
	TxPool() *core.TxPool
}

// Miner creates blocks and searches for proof-of-work values.
type Miner struct {
	mux      *event.TypeMux
	worker   *worker
	coinbase common.Address
	eth      Backend
	engine   consensus.Engine
	exitCh   chan struct{}

	canStart    int32 // can start indicates whether we can start the mining operation
	shouldStart int32 // should start indicates whether we should start after sync
}

func New(eth Backend, config *params.ChainConfig, mux *event.TypeMux, engine consensus.Engine, recommit time.Duration, gasFloor, gasCeil uint64, isLocalBlock func(block *types.Block) bool) *Miner {
	miner := &Miner{
		eth:      eth,
		mux:      mux,
		engine:   engine,
		exitCh:   make(chan struct{}),
		worker:   newWorker(config, engine, eth, mux, recommit, gasFloor, gasCeil, isLocalBlock),
		canStart: 1,
	}
	go miner.update()

	return miner
}

// update keeps track of the downloader events. Please be aware that this is a one shot type of update loop.
// It's entered once and as soon as `Done` or `Failed` has been broadcasted the events are unregistered and
// the loop is exited. This to prevent a major security vuln where external parties can DOS you with blocks
// and halt your mining operation for as long as the DOS continues.
func (mnr *Miner) update() {
	events := mnr.mux.Subscribe(downloader.StartEvent{}, downloader.DoneEvent{}, downloader.FailedEvent{})
	defer events.Unsubscribe()

	for {
		select {
		case ev := <-events.Chan():
			if ev == nil {
				return
			}
			switch ev.Data.(type) {
			case downloader.StartEvent:
				atomic.StoreInt32(&mnr.canStart, 0)
				if mnr.Mining() {
					mnr.Stop()
					atomic.StoreInt32(&mnr.shouldStart, 1)
					log.Info("Mining aborted due to sync")
				}
			case downloader.DoneEvent, downloader.FailedEvent:
				shouldStart := atomic.LoadInt32(&mnr.shouldStart) == 1

				atomic.StoreInt32(&mnr.canStart, 1)
				atomic.StoreInt32(&mnr.shouldStart, 0)
				if shouldStart {
					mnr.Start(mnr.coinbase)
				}
				// stop immediately and ignore all further pending events
				return
			}
		case <-mnr.exitCh:
			return
		}
	}
}

func (mnr *Miner) Start(coinbase common.Address) {
	atomic.StoreInt32(&mnr.shouldStart, 1)
	mnr.SetEtherbase(coinbase)

	if atomic.LoadInt32(&mnr.canStart) == 0 {
		log.Info("Network syncing, will start miner afterwards")
		return
	}
	mnr.worker.start()
}

func (mnr *Miner) Stop() {
	mnr.worker.stop()
	atomic.StoreInt32(&mnr.shouldStart, 0)
}

func (mnr *Miner) Close() {
	mnr.worker.close()
	close(mnr.exitCh)
}

func (mnr *Miner) Mining() bool {
	return mnr.worker.isRunning()
}

func (mnr *Miner) HashRate() uint64 {
	if pow, ok := mnr.engine.(consensus.PoW); ok {
		return uint64(pow.Hashrate())
	}
	return 0
}

func (mnr *Miner) SetExtra(extra []byte) error {
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("Extra exceeds max length. %d > %v", len(extra), params.MaximumExtraDataSize)
	}
	mnr.worker.setExtra(extra)
	return nil
}

// SetRecommitInterval sets the interval for sealing work resubmitting.
func (mnr *Miner) SetRecommitInterval(interval time.Duration) {
	mnr.worker.setRecommitInterval(interval)
}

// Pending returns the currently pending block and associated state.
func (mnr *Miner) Pending() (*types.Block, *state.IntraBlockState, *state.TrieDbState) {
	return mnr.worker.pending()
}

// PendingBlock returns the currently pending block.
//
// Note, to access both the pending block and the pending state
// simultaneously, please use Pending(), as the pending state can
// change between multiple method calls
func (mnr *Miner) PendingBlock() *types.Block {
	return mnr.worker.pendingBlock()
}

func (mnr *Miner) SetEtherbase(addr common.Address) {
	mnr.coinbase = addr
	mnr.worker.setEtherbase(addr)
}
