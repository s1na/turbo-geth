// Copyright 2019 The go-ethereum Authors
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

package state

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/ledgerwatch/turbo-geth/common/debug"
	"io"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"unsafe"

	lru "github.com/hashicorp/golang-lru"

	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/common/dbutils"
	"github.com/ledgerwatch/turbo-geth/core/types/accounts"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/ledgerwatch/turbo-geth/trie"
)

// Trie cache generation limit after which to evict trie nodes from memory.
var MaxTrieCacheGen = uint32(1024 * 1024)

const (
	IncarnationLength = 8
	//FirstContractIncarnation - first incarnation for contract accounts. After 1 it increases by 1.
	FirstContractIncarnation = 1
	//NonContractIncarnation incarnation for non contracts
	NonContractIncarnation = 0
)

type StateReader interface {
	ReadAccountData(address common.Address) (*accounts.Account, error)
	ReadAccountStorage(address common.Address, incarnation uint64, key *common.Hash) ([]byte, error)
	ReadAccountCode(address common.Address, codeHash common.Hash) ([]byte, error)
	ReadAccountCodeSize(address common.Address, codeHash common.Hash) (int, error)
}

type StateWriter interface {
	UpdateAccountData(ctx context.Context, address common.Address, original, account *accounts.Account) error
	UpdateAccountCode(addrHash common.Hash, incarnation uint64, codeHash common.Hash, code []byte) error
	DeleteAccount(ctx context.Context, address common.Address, original *accounts.Account) error
	WriteAccountStorage(ctx context.Context, address common.Address, incarnation uint64, key, original, value *common.Hash) error
	CreateContract(address common.Address) error
}

type NoopWriter struct {
}

func NewNoopWriter() *NoopWriter {
	return &NoopWriter{}
}

func (nw *NoopWriter) UpdateAccountData(_ context.Context, address common.Address, original, account *accounts.Account) error {
	return nil
}

func (nw *NoopWriter) DeleteAccount(_ context.Context, address common.Address, original *accounts.Account) error {
	return nil
}

func (nw *NoopWriter) UpdateAccountCode(addrHash common.Hash, incarnation uint64, codeHash common.Hash, code []byte) error {
	return nil
}

func (nw *NoopWriter) WriteAccountStorage(_ context.Context, address common.Address, incarnation uint64, key, original, value *common.Hash) error {
	return nil
}

func (nw *NoopWriter) CreateContract(address common.Address) error {
	return nil
}

// Structure holding updates, deletes, and reads registered within one change period
// A change period can be transaction within a block, or a block within group of blocks
type Buffer struct {
	storageUpdates map[common.Hash]map[common.Hash][]byte
	storageReads   map[common.Hash]map[common.Hash]struct{}
	accountUpdates map[common.Hash]*accounts.Account
	accountReads   map[common.Hash]struct{}
	deleted        map[common.Hash]struct{}
	created        map[common.Hash]struct{}
}

// Prepares buffer for work or clears previous data
func (b *Buffer) initialise() {
	b.storageUpdates = make(map[common.Hash]map[common.Hash][]byte)
	b.storageReads = make(map[common.Hash]map[common.Hash]struct{})
	b.accountUpdates = make(map[common.Hash]*accounts.Account)
	b.accountReads = make(map[common.Hash]struct{})
	b.deleted = make(map[common.Hash]struct{})
	b.created = make(map[common.Hash]struct{})
}

// Replaces account pointer with pointers to the copies
func (b *Buffer) detachAccounts() {
	for addrHash, account := range b.accountUpdates {
		if account != nil {
			b.accountUpdates[addrHash] = account.SelfCopy()
		}
	}
}

// Merges the content of another buffer into this one
func (b *Buffer) merge(other *Buffer) {
	for addrHash, om := range other.storageUpdates {
		m, ok := b.storageUpdates[addrHash]
		if !ok {
			m = make(map[common.Hash][]byte)
			b.storageUpdates[addrHash] = m
		}
		for keyHash, v := range om {
			m[keyHash] = v
		}
	}
	for addrHash, om := range other.storageReads {
		m, ok := b.storageReads[addrHash]
		if !ok {
			m = make(map[common.Hash]struct{})
			b.storageReads[addrHash] = m
		}
		for keyHash := range om {
			m[keyHash] = struct{}{}
		}
	}
	for addrHash, account := range other.accountUpdates {
		b.accountUpdates[addrHash] = account
	}
	for addrHash := range other.accountReads {
		b.accountReads[addrHash] = struct{}{}
	}
	for addrHash := range other.deleted {
		b.deleted[addrHash] = struct{}{}
	}
	for addrHash := range other.created {
		b.created[addrHash] = struct{}{}
	}
}

// TrieDbState implements StateReader by wrapping a trie and a database, where trie acts as a cache for the database
type TrieDbState struct {
	t               *trie.Trie
	tMu             *sync.Mutex
	db              ethdb.Database
	blockNr         uint64
	buffers         []*Buffer
	aggregateBuffer *Buffer // Merge of all buffers
	currentBuffer   *Buffer
	codeCache       *lru.Cache
	codeSizeCache   *lru.Cache
	historical      bool
	noHistory       bool
	resolveReads    bool
	savePreimages   bool
	pg              *trie.ProofGenerator
	tp              *trie.TriePruning
}

var (
	trieObj   = make(map[uint64]uintptr)
	trieObjMu sync.RWMutex
)

func getTrieDBState(db ethdb.Database) *TrieDbState {
	if db == nil {
		return nil
	}
	trieObjMu.RLock()
	tr, ok := trieObj[db.ID()]
	trieObjMu.RUnlock()

	if !ok {
		return nil
	}

	return (*TrieDbState)(unsafe.Pointer(tr))
}

func setTrieDBState(tds *TrieDbState, id uint64) {
	if tds == nil {
		return
	}

	ptr := unsafe.Pointer(tds)

	trieObjMu.Lock()
	trieObj[id] = uintptr(ptr)
	trieObjMu.Unlock()
}

func NewTrieDbState(root common.Hash, db ethdb.Database, blockNr uint64) (*TrieDbState, error) {
	tds, err := newTrieDbState(root, db, blockNr)
	if err != nil {
		return nil, err
	}

	setTrieDBState(tds, db.ID())

	return tds, nil
}

func newTrieDbState(root common.Hash, db ethdb.Database, blockNr uint64) (*TrieDbState, error) {
	csc, err := lru.New(100000)
	if err != nil {
		return nil, err
	}
	cc, err := lru.New(10000)
	if err != nil {
		return nil, err
	}
	t := trie.New(root)
	tp := trie.NewTriePruning(blockNr)

	tds := &TrieDbState{
		t:             t,
		tMu:           new(sync.Mutex),
		db:            db,
		blockNr:       blockNr,
		codeCache:     cc,
		codeSizeCache: csc,
		pg:            trie.NewProofGenerator(),
		tp:            tp,
		savePreimages: true,
	}
	t.SetTouchFunc(func(hex []byte, del bool) {
		tp.Touch(hex, del)
	})

	return tds, nil
}

func GetTrieDbState(root common.Hash, db ethdb.Database, blockNr uint64) (*TrieDbState, error) {
	if tr := getTrieDBState(db); tr != nil {
		if tr.getBlockNr() == blockNr && tr.LastRoot() == root {
			return tr, nil
		}
	}

	return newTrieDbState(root, db, blockNr)
}

func (tds *TrieDbState) EnablePreimages(ep bool) {
	tds.savePreimages = ep
}

func (tds *TrieDbState) SetHistorical(h bool) {
	tds.historical = h
}

func (tds *TrieDbState) SetResolveReads(rr bool) {
	tds.resolveReads = rr
}

func (tds *TrieDbState) SetNoHistory(nh bool) {
	tds.noHistory = nh
}

func (tds *TrieDbState) Copy() *TrieDbState {
	tds.tMu.Lock()
	tcopy := *tds.t
	tds.tMu.Unlock()

	n := tds.getBlockNr()
	tp := trie.NewTriePruning(n)

	cpy := TrieDbState{
		t:       &tcopy,
		tMu:     new(sync.Mutex),
		db:      tds.db,
		blockNr: n,
		tp:      tp,
	}
	return &cpy
}

func (tds *TrieDbState) Database() ethdb.Database {
	return tds.db
}

func (tds *TrieDbState) Trie() *trie.Trie {
	return tds.t
}

func (tds *TrieDbState) StartNewBuffer() {
	if tds.currentBuffer != nil {
		if tds.aggregateBuffer == nil {
			tds.aggregateBuffer = &Buffer{}
			tds.aggregateBuffer.initialise()
		}
		tds.aggregateBuffer.merge(tds.currentBuffer)
		tds.currentBuffer.detachAccounts()
	}
	tds.currentBuffer = &Buffer{}
	tds.currentBuffer.initialise()
	tds.buffers = append(tds.buffers, tds.currentBuffer)
}

func (tds *TrieDbState) WithNewBuffer() *TrieDbState {
	aggregateBuffer := &Buffer{}
	aggregateBuffer.initialise()

	currentBuffer := &Buffer{}
	currentBuffer.initialise()

	buffers := []*Buffer{currentBuffer}

	tds.tMu.Lock()
	t := &TrieDbState{
		t:               tds.t,
		tMu:             tds.tMu,
		db:              tds.db,
		blockNr:         tds.getBlockNr(),
		buffers:         buffers,
		aggregateBuffer: aggregateBuffer,
		currentBuffer:   currentBuffer,
		codeCache:       tds.codeCache,
		codeSizeCache:   tds.codeSizeCache,
		historical:      tds.historical,
		noHistory:       tds.noHistory,
		resolveReads:    tds.resolveReads,
		pg:              tds.pg,
		tp:              tds.tp,
	}
	tds.tMu.Unlock()

	return t
}

func (tds *TrieDbState) LastRoot() common.Hash {
	tds.tMu.Lock()
	defer tds.tMu.Unlock()
	return tds.t.Hash()
}

// ComputeTrieRoots is a combination of `ResolveStateTrie` and `UpdateStateTrie`
// DESCRIBED: docs/programmers_guide/guide.md#organising-ethereum-state-into-a-merkle-tree
func (tds *TrieDbState) ComputeTrieRoots() ([]common.Hash, error) {
	if err := tds.ResolveStateTrie(); err != nil {
		return nil, err
	}
	return tds.UpdateStateTrie()
}

// UpdateStateTrie assumes that the state trie is already fully resolved, i.e. any operations
// will find necessary data inside the trie.
func (tds *TrieDbState) UpdateStateTrie() ([]common.Hash, error) {
	tds.tMu.Lock()
	defer tds.tMu.Unlock()

	roots, err := tds.updateTrieRoots(true)
	tds.clearUpdates()
	return roots, err
}

func (tds *TrieDbState) PrintTrie(w io.Writer) {
	tds.tMu.Lock()
	defer tds.tMu.Unlock()
	tds.t.Print(w)
	fmt.Fprintln(w, "") //nolint
}

// Builds a map where for each address (of a smart contract) there is
// a sorted list of all key hashes that were touched within the
// period for which we are aggregating updates
func (tds *TrieDbState) buildStorageTouches(withReads bool, withValues bool) (common.StorageKeys, [][]byte) {
	storageTouches := common.StorageKeys{}
	var values [][]byte
	for addrHash, m := range tds.aggregateBuffer.storageUpdates {
		if withValues {
			if _, ok := tds.aggregateBuffer.deleted[addrHash]; ok {
				continue
			}
		}
		for keyHash := range m {
			var storageKey common.StorageKey
			copy(storageKey[:], addrHash[:])
			copy(storageKey[common.HashLength:], keyHash[:])
			storageTouches = append(storageTouches, storageKey)
		}
	}
	if withReads {
		for addrHash, m := range tds.aggregateBuffer.storageReads {
			mWrite := tds.aggregateBuffer.storageUpdates[addrHash]
			for keyHash := range m {
				if mWrite != nil {
					if _, ok := mWrite[keyHash]; ok {
						// Avoid repeating the same storage keys if they are both read and updated
						continue
					}
				}
				var storageKey common.StorageKey
				copy(storageKey[:], addrHash[:])
				copy(storageKey[common.HashLength:], keyHash[:])
				storageTouches = append(storageTouches, storageKey)
			}
		}
	}
	sort.Sort(storageTouches)
	if withValues {
		// We assume that if withValues == true, then withReads == false
		var addrHash common.Hash
		var keyHash common.Hash
		for _, storageKey := range storageTouches {
			copy(addrHash[:], storageKey[:])
			copy(keyHash[:], storageKey[common.HashLength:])
			values = append(values, tds.aggregateBuffer.storageUpdates[addrHash][keyHash])
		}
	}
	return storageTouches, values
}

// Expands the storage tries (by loading data from the database) if it is required
// for accessing storage slots containing in the storageTouches map
func (tds *TrieDbState) resolveStorageTouches(storageTouches common.StorageKeys) error {
	var resolver *trie.Resolver
	for _, storageKey := range storageTouches {
		if need, req := tds.t.NeedResolution(storageKey[:common.HashLength], storageKey[:]); need {
			if resolver == nil {
				resolver = trie.NewResolver(0, false, tds.blockNr)
				resolver.SetHistorical(tds.historical)
			}
			resolver.AddRequest(req)
		}
	}
	if resolver != nil {
		if err := resolver.ResolveWithDb(tds.db, tds.blockNr); err != nil {
			return err
		}
	}
	return nil
}

// Populate pending block proof so that it will be sufficient for accessing all storage slots in storageTouches
func (tds *TrieDbState) populateStorageBlockProof(storageTouches common.StorageKeys) error { //nolint
	for _, storageKey := range storageTouches {
		tds.pg.AddStorageTouch(storageKey[:])
	}
	return nil
}

// Builds a sorted list of all address hashes that were touched within the
// period for which we are aggregating updates
func (tds *TrieDbState) buildAccountTouches(withReads bool, withValues bool) (common.Hashes, []*accounts.Account) {
	accountTouches := common.Hashes{}
	var aValues []*accounts.Account
	for addrHash, aValue := range tds.aggregateBuffer.accountUpdates {
		if aValue != nil {
			if _, ok := tds.aggregateBuffer.deleted[addrHash]; ok {
				accountTouches = append(accountTouches, addrHash)
			}
		}
		accountTouches = append(accountTouches, addrHash)
	}
	if withReads {
		for addrHash := range tds.aggregateBuffer.accountReads {
			if _, ok := tds.aggregateBuffer.accountUpdates[addrHash]; !ok {
				accountTouches = append(accountTouches, addrHash)
			}
		}
	}
	sort.Sort(accountTouches)
	if withValues {
		// We assume that if withValues == true, then withReads == false
		aValues = make([]*accounts.Account, len(accountTouches))
		for i, addrHash := range accountTouches {
			if i < len(accountTouches)-1 && addrHash == accountTouches[i+1] {
				aValues[i] = nil // Entry that would wipe out existing storage
			} else {
				a := tds.aggregateBuffer.accountUpdates[addrHash]
				if a != nil {
					if _, ok := tds.aggregateBuffer.storageUpdates[addrHash]; ok {
						var ac accounts.Account
						ac.Copy(a)
						ac.Root = trie.EmptyRoot
						a = &ac
					}
				}
				aValues[i] = a
			}
		}
	}
	return accountTouches, aValues
}

// Expands the accounts trie (by loading data from the database) if it is required
// for accessing accounts whose addresses are contained in the accountTouches
func (tds *TrieDbState) resolveAccountTouches(accountTouches common.Hashes) error {
	var resolver *trie.Resolver
	for _, addrHash := range accountTouches {
		if need, req := tds.t.NeedResolution(nil, addrHash[:]); need {
			if resolver == nil {
				resolver = trie.NewResolver(0, true, tds.blockNr)
				resolver.SetHistorical(tds.historical)
			}
			resolver.AddRequest(req)
		}
	}
	if resolver != nil {
		if err := resolver.ResolveWithDb(tds.db, tds.blockNr); err != nil {
			return err
		}
		resolver = nil
	}
	return nil
}

func (tds *TrieDbState) populateAccountBlockProof(accountTouches common.Hashes) {
	for _, addrHash := range accountTouches {
		a := addrHash
		tds.pg.AddTouch(a[:])
	}
}

// ExtractTouches returns two lists of keys - for accounts and storage items correspondingly
// Each list is the collection of keys that have been "touched" (inserted, updated, or simply accessed)
// since the last invocation of `ExtractTouches`.
func (tds *TrieDbState) ExtractTouches() (accountTouches [][]byte, storageTouches [][]byte) {
	return tds.pg.ExtractTouches()
}

// ResolveStateTrie resolves parts of the state trie that would be necessary for any updates
// (and reads, if `resolveReads` is set).
func (tds *TrieDbState) ResolveStateTrie() error {
	// Aggregating the current buffer, if any
	if tds.currentBuffer != nil {
		if tds.aggregateBuffer == nil {
			tds.aggregateBuffer = &Buffer{}
			tds.aggregateBuffer.initialise()
		}
		tds.aggregateBuffer.merge(tds.currentBuffer)
	}
	if tds.aggregateBuffer == nil {
		return nil
	}

	tds.tMu.Lock()
	defer tds.tMu.Unlock()

	// Prepare (resolve) storage tries so that actual modifications can proceed without database access
	storageTouches, _ := tds.buildStorageTouches(tds.resolveReads, false)

	// Prepare (resolve) accounts trie so that actual modifications can proceed without database access
	accountTouches, _ := tds.buildAccountTouches(tds.resolveReads, false)
	if err := tds.resolveAccountTouches(accountTouches); err != nil {
		return err
	}

	if tds.resolveReads {
		tds.populateAccountBlockProof(accountTouches)
	}

	if err := tds.resolveStorageTouches(storageTouches); err != nil {
		return err
	}
	if tds.resolveReads {
		if err := tds.populateStorageBlockProof(storageTouches); err != nil {
			return err
		}
	}
	return nil
}

// CalcTrieRoots calculates trie roots without modifying the state trie
func (tds *TrieDbState) CalcTrieRoots(trace bool) (common.Hash, error) {
	tds.tMu.Lock()
	defer tds.tMu.Unlock()

	// Retrive the list of inserted/updated/deleted storage items (keys and values)
	storageKeys, sValues := tds.buildStorageTouches(false, true)
	if trace {
		fmt.Printf("len(storageKeys)=%d, len(sValues)=%d\n", len(storageKeys), len(sValues))
	}
	// Retrive the list of inserted/updated/deleted accounts (keys and values)
	accountKeys, aValues := tds.buildAccountTouches(false, true)
	if trace {
		fmt.Printf("len(accountKeys)=%d, len(aValues)=%d\n", len(accountKeys), len(aValues))
	}
	return trie.HashWithModifications(tds.t, accountKeys, aValues, storageKeys, sValues, common.HashLength, trace)
}

// forward is `true` if the function is used to progress the state forward (by adding blocks)
// forward is `false` if the function is used to rewind the state (for reorgs, for example)
func (tds *TrieDbState) updateTrieRoots(forward bool) ([]common.Hash, error) {
	accountUpdates := tds.aggregateBuffer.accountUpdates
	// Perform actual updates on the tries, and compute one trie root per buffer
	// These roots can be used to populate receipt.PostState on pre-Byzantium
	roots := make([]common.Hash, len(tds.buffers))
	// The following map is to prevent repeated clearouts of the storage
	alreadyCreated := make(map[common.Hash]struct{})
	for i, b := range tds.buffers {
		// New contracts are being created at these addresses. Therefore, we need to clear the storage items
		// that might be remaining in the trie and figure out the next incarnations
		for addrHash := range b.created {
			// Prevent repeated storage clearouts
			if _, ok := alreadyCreated[addrHash]; ok {
				continue
			}
			alreadyCreated[addrHash] = struct{}{}
			if account, ok := b.accountUpdates[addrHash]; ok && account != nil {
				b.accountUpdates[addrHash].Root = trie.EmptyRoot
			}
			if account, ok := tds.aggregateBuffer.accountUpdates[addrHash]; ok && account != nil {
				tds.aggregateBuffer.accountUpdates[addrHash].Root = trie.EmptyRoot
			}
			// The only difference between Delete and DeleteSubtree is that Delete would delete accountNode too,
			// wherewas DeleteSubtree will keep the accountNode, but will make the storage sub-trie empty
			tds.t.DeleteSubtree(addrHash[:], tds.blockNr)
		}

		for addrHash, account := range b.accountUpdates {
			if account != nil {
				//fmt.Println("b.accountUpdates",addrHash.String(), account.Incarnation)
				tds.t.UpdateAccount(addrHash[:], account)
			} else {
				tds.t.Delete(addrHash[:], tds.blockNr)
			}
		}
		for addrHash, m := range b.storageUpdates {
			for keyHash, v := range m {
				cKey := dbutils.GenerateCompositeTrieKey(addrHash, keyHash)
				if len(v) > 0 {
					//fmt.Printf("Update storage trie addrHash %x, keyHash %x: %x\n", addrHash, keyHash, v)
					if forward {
						tds.t.Update(cKey, v, tds.blockNr)
					} else {
						// If rewinding, it might not be possible to execute storage item update.
						// If we rewind from the state where a contract does not exist anymore (it was self-destructed)
						// to the point where it existed (with storage), then rewinding to the point of existence
						// will not bring back the full storage trie. Instead there will be one hashNode.
						// So we probe for this situation first
						if _, ok := tds.t.Get(cKey); ok {
							tds.t.Update(cKey, v, tds.blockNr)
						}
					}
				} else {
					//fmt.Printf("Delete storage trie addrHash %x, keyHash %x\n", addrHash, keyHash)
					if forward {
						tds.t.Delete(cKey, tds.blockNr)
					} else {
						// If rewinding, it might not be possible to execute storage item update.
						// If we rewind from the state where a contract does not exist anymore (it was self-destructed)
						// to the point where it existed (with storage), then rewinding to the point of existence
						// will not bring back the full storage trie. Instead there will be one hashNode.
						// So we probe for this situation first
						if _, ok := tds.t.Get(cKey); ok {
							tds.t.Delete(cKey, tds.blockNr)
						}
					}
				}
			}

			if forward || debug.IsThinHistory() {
				if account, ok := b.accountUpdates[addrHash]; ok && account != nil {
					ok, root := tds.t.DeepHash(addrHash[:])
					if ok {
						account.Root = root
						//fmt.Printf("(b)Set %x root for addrHash %x\n", root, addrHash)
					} else {
						//fmt.Printf("(b)Set empty root for addrHash %x\n", addrHash)
						account.Root = trie.EmptyRoot
					}
				}
				if account, ok := accountUpdates[addrHash]; ok && account != nil {
					ok, root := tds.t.DeepHash(addrHash[:])
					if ok {
						account.Root = root
						//fmt.Printf("Set %x root for addrHash %x\n", root, addrHash)
					} else {
						//fmt.Printf("Set empty root for addrHash %x\n", addrHash)
						account.Root = trie.EmptyRoot
					}
				}
			} else {
				// Simply comparing the correctness of the storageRoot computations
				if account, ok := b.accountUpdates[addrHash]; ok && account != nil {
					ok, h := tds.t.DeepHash(addrHash[:])
					if !ok {
						h = trie.EmptyRoot
					}

					if account.Root != h {
						return nil, fmt.Errorf("mismatched storage root for %x: expected %x, got %x", addrHash, account.Root, h)
					}
				}
				if account, ok := accountUpdates[addrHash]; ok && account != nil {
					ok, h := tds.t.DeepHash(addrHash[:])
					if !ok {
						h = trie.EmptyRoot
					}

					if account.Root != h {
						return nil, fmt.Errorf("mismatched storage root for %x: expected %x, got %x", addrHash, account.Root, h)
					}
				}
			}
		}

		// For the contracts that got deleted
		for addrHash := range b.deleted {
			if _, ok := b.created[addrHash]; ok {
				// In some rather artificial circumstances, an account can be recreated after having been self-destructed
				// in the same block. It can only happen when contract is introduced in the genesis state with nonce 0
				// rather than created by a transaction (in that case, its starting nonce is 1). The self-destructed
				// contract actually gets removed from the state only at the end of the block, so if its nonce is not 0,
				// it will prevent any re-creation within the same block. However, if the contract is introduced in
				// the genesis state, its nonce is 0, and that means it can be self-destructed, and then re-created,
				// all in the same block. In such cases, we must preserve storage modifications happening after the
				// self-destruction
				continue
			}
			if account, ok := b.accountUpdates[addrHash]; ok && account != nil {
				//fmt.Printf("(b)Set empty root for addrHash %x due to deleted\n", addrHash)
				account.Root = trie.EmptyRoot
			}
			if account, ok := accountUpdates[addrHash]; ok && account != nil {
				//fmt.Printf("Set empty root for addrHash %x due to deleted\n", addrHash)
				account.Root = trie.EmptyRoot
			}
			tds.t.DeleteSubtree(addrHash[:], tds.blockNr)
		}
		roots[i] = tds.t.Hash()
	}
	return roots, nil
}

func (tds *TrieDbState) clearUpdates() {
	tds.buffers = nil
	tds.currentBuffer = nil
	tds.aggregateBuffer = nil
}

func (tds *TrieDbState) Rebuild() error {
	tds.tMu.Lock()
	defer tds.tMu.Unlock()
	err := tds.t.Rebuild(tds.db, tds.blockNr)
	if err != nil {
		return err
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	log.Info("Memory after rebuild", "nodes", tds.tp.NodeCount(), "alloc", int(m.Alloc/1024), "sys", int(m.Sys/1024), "numGC", int(m.NumGC))
	return nil
}

func (tds *TrieDbState) SetBlockNr(blockNr uint64) {
	tds.setBlockNr(blockNr)
	tds.tp.SetBlockNr(blockNr)
}

func (tds *TrieDbState) GetBlockNr() uint64 {
	return tds.getBlockNr()
}

func (tds *TrieDbState) UnwindTo(blockNr uint64) error {
	tds.StartNewBuffer()
	b := tds.currentBuffer

	if err := tds.db.RewindData(tds.blockNr, blockNr, func(bucket, key, value []byte) error {
		//fmt.Printf("bucket: %x, key: %x, value: %x\n", bucket, key, value)
		if bytes.Equal(bucket, dbutils.AccountsHistoryBucket) {
			var addrHash common.Hash
			copy(addrHash[:], key)
			if len(value) > 0 {
				var acc accounts.Account
				if err := acc.DecodeForStorage(value); err != nil {
					return err
				}
				// Fetch the code hash
				if acc.Incarnation > 0 && debug.IsThinHistory() && acc.IsEmptyCodeHash() {
					if codeHash, err := tds.db.Get(dbutils.ContractCodeBucket, dbutils.GenerateStoragePrefix(addrHash, acc.Incarnation)); err != nil {
						copy(acc.CodeHash[:], codeHash)
					}
				}
				b.accountUpdates[addrHash] = &acc
				if err := tds.db.Put(dbutils.AccountsBucket, addrHash[:], value); err != nil {
					return err
				}
			} else {
				b.accountUpdates[addrHash] = nil
				if err := tds.db.Delete(dbutils.AccountsBucket, addrHash[:]); err != nil {
					return err
				}
			}
		} else if bytes.Equal(bucket, dbutils.StorageHistoryBucket) {
			var addrHash common.Hash
			copy(addrHash[:], key[:common.HashLength])
			var keyHash common.Hash
			copy(keyHash[:], key[common.HashLength+IncarnationLength:])
			m, ok := b.storageUpdates[addrHash]
			if !ok {
				m = make(map[common.Hash][]byte)
				b.storageUpdates[addrHash] = m
			}
			if len(value) > 0 {
				m[keyHash] = value
				if err := tds.db.Put(dbutils.StorageBucket, key[:common.HashLength+IncarnationLength+common.HashLength], value); err != nil {
					return err
				}
			} else {
				m[keyHash] = nil
				if err := tds.db.Delete(dbutils.StorageBucket, key[:common.HashLength+IncarnationLength+common.HashLength]); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := tds.ResolveStateTrie(); err != nil {
		return err
	}

	tds.tMu.Lock()
	defer tds.tMu.Unlock()
	if _, err := tds.updateTrieRoots(false); err != nil {
		return err
	}
	for i := tds.blockNr; i > blockNr; i-- {
		if err := tds.db.DeleteTimestamp(i); err != nil {
			return err
		}
	}

	tds.clearUpdates()
	tds.setBlockNr(blockNr)
	return nil
}

func (tds *TrieDbState) readAccountDataByHash(addrHash common.Hash) (*accounts.Account, error) {
	if acc, ok := tds.GetAccount(addrHash); ok {
		return acc, nil
	}

	// Not present in the trie, try the database
	var err error
	var enc []byte
	if tds.historical {
		enc, err = tds.db.GetAsOf(dbutils.AccountsBucket, dbutils.AccountsHistoryBucket, addrHash[:], tds.blockNr+1)
		if err != nil {
			enc = nil
		}
	} else {
		enc, err = tds.db.Get(dbutils.AccountsBucket, addrHash[:])
		if err != nil {
			enc = nil
		}
	}
	if len(enc) == 0 {
		return nil, nil
	}
	var a accounts.Account
	if err := a.DecodeForStorage(enc); err != nil {
		return nil, err
	}

	if tds.historical && debug.IsThinHistory() {
		codeHash, err := tds.db.Get(dbutils.ContractCodeBucket, dbutils.GenerateStoragePrefix(addrHash, a.Incarnation))
		if err == nil {
			a.CodeHash = common.BytesToHash(codeHash)
		} else {
			log.Error("Get code hash is incorrect", "err", err)
		}
	}
	return &a, nil
}

func (tds *TrieDbState) GetAccount(addrHash common.Hash) (*accounts.Account, bool) {
	tds.tMu.Lock()
	defer tds.tMu.Unlock()
	acc, ok := tds.t.GetAccount(addrHash[:])
	return acc, ok
}

func (tds *TrieDbState) ReadAccountData(address common.Address) (*accounts.Account, error) {
	addrHash, err := common.HashData(address[:])
	if err != nil {
		return nil, err
	}
	if tds.resolveReads {
		if _, ok := tds.currentBuffer.accountUpdates[addrHash]; !ok {
			tds.currentBuffer.accountReads[addrHash] = struct{}{}
		}
	}

	return tds.readAccountDataByHash(addrHash)
}

func (tds *TrieDbState) savePreimage(save bool, hash, preimage []byte) error {
	if !save || !tds.savePreimages {
		return nil
	}
	// Following check is to minimise the overwriting the same value of preimage
	// in the database, which would cause extra write churn
	if p, _ := tds.db.Get(dbutils.PreimagePrefix, hash); p != nil {
		return nil
	}
	return tds.db.Put(dbutils.PreimagePrefix, hash, preimage)
}

func (tds *TrieDbState) HashAddress(address common.Address, save bool) (common.Hash, error) {
	addrHash, err := common.HashData(address[:])
	if err != nil {
		return common.Hash{}, err
	}
	return addrHash, tds.savePreimage(save, addrHash[:], address[:])
}

func (tds *TrieDbState) HashKey(key *common.Hash, save bool) (common.Hash, error) {
	keyHash, err := common.HashData(key[:])
	if err != nil {
		return common.Hash{}, err
	}
	return keyHash, tds.savePreimage(save, keyHash[:], key[:])
}

func (tds *TrieDbState) GetKey(shaKey []byte) []byte {
	key, _ := tds.db.Get(dbutils.PreimagePrefix, shaKey)
	return key
}

func (tds *TrieDbState) ReadAccountStorage(address common.Address, incarnation uint64, key *common.Hash) ([]byte, error) {
	addrHash, err := tds.HashAddress(address, false /*save*/)
	if err != nil {
		return nil, err
	}
	if tds.currentBuffer != nil {
		if _, ok := tds.currentBuffer.deleted[addrHash]; ok {
			return nil, nil
		}
	}
	if tds.aggregateBuffer != nil {
		if _, ok := tds.aggregateBuffer.deleted[addrHash]; ok {
			return nil, nil
		}
	}
	seckey, err := tds.HashKey(key, false /*save*/)
	if err != nil {
		return nil, err
	}

	if tds.resolveReads {
		var addReadRecord = false
		if mWrite, ok := tds.currentBuffer.storageUpdates[addrHash]; ok {
			if _, ok1 := mWrite[seckey]; !ok1 {
				addReadRecord = true
			}
		} else {
			addReadRecord = true
		}
		if addReadRecord {
			m, ok := tds.currentBuffer.storageReads[addrHash]
			if !ok {
				m = make(map[common.Hash]struct{})
				tds.currentBuffer.storageReads[addrHash] = m
			}
			m[seckey] = struct{}{}
		}
	}

	tds.tMu.Lock()
	enc, ok := tds.t.Get(dbutils.GenerateCompositeTrieKey(addrHash, seckey))
	defer tds.tMu.Unlock()
	if !ok {
		// Not present in the trie, try database
		if tds.historical {
			enc, err = tds.db.GetAsOf(dbutils.StorageBucket, dbutils.StorageHistoryBucket, dbutils.GenerateCompositeStorageKey(addrHash, incarnation, seckey), tds.blockNr)
			if err != nil {
				enc = nil
			}
		} else {
			enc, err = tds.db.Get(dbutils.StorageBucket, dbutils.GenerateCompositeStorageKey(addrHash, incarnation, seckey))
			if err != nil {
				enc = nil
			}
		}
	}
	return enc, nil
}

func (tds *TrieDbState) ReadAccountCode(address common.Address, codeHash common.Hash) (code []byte, err error) {
	if bytes.Equal(codeHash[:], emptyCodeHash) {
		return nil, nil
	}
	if cached, ok := tds.codeCache.Get(codeHash); ok {
		code, err = cached.([]byte), nil
	} else {
		code, err = tds.db.Get(dbutils.CodeBucket, codeHash[:])
		if err == nil {
			tds.codeSizeCache.Add(codeHash, len(code))
			tds.codeCache.Add(codeHash, code)
		}
	}
	if tds.resolveReads {
		addrHash, err1 := common.HashData(address[:])
		if err1 != nil {
			return nil, err
		}
		if _, ok := tds.currentBuffer.accountUpdates[addrHash]; !ok {
			tds.currentBuffer.accountReads[addrHash] = struct{}{}
		}
		tds.pg.ReadCode(codeHash, code)
	}
	return code, err
}

func (tds *TrieDbState) ReadAccountCodeSize(address common.Address, codeHash common.Hash) (codeSize int, err error) {
	var code []byte
	if cached, ok := tds.codeSizeCache.Get(codeHash); ok {
		codeSize, err = cached.(int), nil
		if tds.resolveReads {
			if cachedCode, ok := tds.codeCache.Get(codeHash); ok {
				code, err = cachedCode.([]byte), nil
			} else {
				code, err = tds.ReadAccountCode(address, codeHash)
				if err != nil {
					return 0, err
				}
			}
		}
	} else {
		code, err = tds.ReadAccountCode(address, codeHash)
		if err != nil {
			return 0, err
		}
		codeSize = len(code)
	}
	if tds.resolveReads {
		addrHash, err1 := common.HashData(address[:])
		if err1 != nil {
			return 0, err
		}
		if _, ok := tds.currentBuffer.accountUpdates[addrHash]; !ok {
			tds.currentBuffer.accountReads[addrHash] = struct{}{}
		}
		tds.pg.ReadCode(codeHash, code)
	}
	return codeSize, nil
}

// nextIncarnation determines what should be the next incarnation of an account (i.e. how many time it has existed before at this address)
func (tds *TrieDbState) nextIncarnation(addrHash common.Hash) (uint64, error) {
	var found bool
	var incarnationBytes [IncarnationLength]byte
	if tds.historical {
		// We reserve ethdb.MaxTimestampLength (8) at the end of the key to accomodate any possible timestamp
		// (timestamp's encoding may have variable length)
		startkey := make([]byte, common.HashLength+IncarnationLength+common.HashLength+ethdb.MaxTimestampLength)
		var fixedbits uint = 8 * common.HashLength
		copy(startkey, addrHash[:])
		if err := tds.db.WalkAsOf(dbutils.StorageBucket, dbutils.StorageHistoryBucket, startkey, fixedbits, tds.blockNr, func(k, _ []byte) (bool, error) {
			copy(incarnationBytes[:], k[common.HashLength:])
			found = true
			return false, nil
		}); err != nil {
			return 0, err
		}
	} else {
		startkey := make([]byte, common.HashLength+IncarnationLength+common.HashLength)
		var fixedbits uint = 8 * common.HashLength
		copy(startkey, addrHash[:])
		if err := tds.db.Walk(dbutils.StorageBucket, startkey, fixedbits, func(k, v []byte) (bool, error) {
			copy(incarnationBytes[:], k[common.HashLength:])
			found = true
			return false, nil
		}); err != nil {
			return 0, err
		}
	}
	if found {
		return (^uint64(0) ^ binary.BigEndian.Uint64(incarnationBytes[:])) + 1, nil
	}
	return FirstContractIncarnation, nil
}

var prevMemStats runtime.MemStats

type TrieStateWriter struct {
	tds *TrieDbState
}

func (tds *TrieDbState) PruneTries(print bool) {
	tds.tMu.Lock()
	defer tds.tMu.Unlock()
	if print {
		prunableNodes := tds.t.CountPrunableNodes()
		fmt.Printf("[Before] Actual prunable nodes: %d, accounted: %d\n", prunableNodes, tds.tp.NodeCount())
	}

	tds.tp.PruneTo(tds.t, int(MaxTrieCacheGen))

	if print {
		prunableNodes := tds.t.CountPrunableNodes()
		fmt.Printf("[After] Actual prunable nodes: %d, accounted: %d\n", prunableNodes, tds.tp.NodeCount())
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	log.Info("Memory", "nodes", tds.tp.NodeCount(), "alloc", int(m.Alloc/1024), "sys", int(m.Sys/1024), "numGC", int(m.NumGC))
	if print {
		fmt.Printf("Pruning done. Nodes: %d, alloc: %d, sys: %d, numGC: %d\n", tds.tp.NodeCount(), int(m.Alloc/1024), int(m.Sys/1024), int(m.NumGC))
	}
}

func (tds *TrieDbState) TrieStateWriter() *TrieStateWriter {
	return &TrieStateWriter{tds: tds}
}

func (tds *TrieDbState) DbStateWriter() *DbStateWriter {
	return &DbStateWriter{tds: tds}
}

func accountsEqual(a1, a2 *accounts.Account) bool {
	if a1.Nonce != a2.Nonce {
		return false
	}
	if !a1.Initialised {
		if a2.Initialised {
			return false
		}
	} else if !a2.Initialised {
		return false
	} else if a1.Balance.Cmp(&a2.Balance) != 0 {
		return false
	}
	if a1.Root != a2.Root {
		return false
	}
	if a1.CodeHash == (common.Hash{}) {
		if a2.CodeHash != (common.Hash{}) {
			return false
		}
	} else if a2.CodeHash == (common.Hash{}) {
		return false
	} else if a1.CodeHash != a2.CodeHash {
		return false
	}
	return true
}

func (tsw *TrieStateWriter) UpdateAccountData(_ context.Context, address common.Address, original, account *accounts.Account) error {
	addrHash, err := tsw.tds.HashAddress(address, false /*save*/)
	if err != nil {
		return err
	}

	tsw.tds.currentBuffer.accountUpdates[addrHash] = account
	return nil
}

func (tsw *TrieStateWriter) DeleteAccount(_ context.Context, address common.Address, original *accounts.Account) error {
	addrHash, err := tsw.tds.HashAddress(address, false /*save*/)
	if err != err {
		return err
	}
	tsw.tds.currentBuffer.accountUpdates[addrHash] = nil
	delete(tsw.tds.currentBuffer.storageUpdates, addrHash)
	tsw.tds.currentBuffer.deleted[addrHash] = struct{}{}
	return nil
}

func (tsw *TrieStateWriter) UpdateAccountCode(addrHash common.Hash, incarnation uint64, codeHash common.Hash, code []byte) error {
	if tsw.tds.resolveReads {
		tsw.tds.pg.CreateCode(codeHash, code)
	}
	return nil
}

func (tsw *TrieStateWriter) WriteAccountStorage(_ context.Context, address common.Address, incarnation uint64, key, original, value *common.Hash) error {
	addrHash, err := tsw.tds.HashAddress(address, false /*save*/)
	if err != nil {
		return err
	}

	v := bytes.TrimLeft(value[:], "\x00")
	m, ok := tsw.tds.currentBuffer.storageUpdates[addrHash]
	if !ok {
		m = make(map[common.Hash][]byte)
		tsw.tds.currentBuffer.storageUpdates[addrHash] = m
	}
	seckey, err := tsw.tds.HashKey(key, false /*save*/)
	if err != nil {
		return err
	}
	if len(v) > 0 {
		m[seckey] = v
	} else {
		m[seckey] = nil
	}
	//fmt.Printf("WriteAccountStorage %x %x: %x, buffer %d\n", addrHash, seckey, value, len(tsw.tds.buffers))
	return nil
}

// ExtractWitness produces block witness for the block just been processed, in a serialised form
func (tds *TrieDbState) ExtractWitness(trace bool, bin bool) ([]byte, *BlockWitnessStats, error) {
	bwb := trie.NewBlockWitnessBuilder(trace)

	var rs *trie.ResolveSet
	if bin {
		rs = trie.NewBinaryResolveSet(0)
	} else {
		rs = trie.NewResolveSet(0)
	}

	touches, storageTouches := tds.pg.ExtractTouches()
	for _, touch := range touches {
		rs.AddKey(touch)
	}
	for _, touch := range storageTouches {
		rs.AddKey(touch)
	}
	codeMap := tds.pg.ExtractCodeMap()

	err := tds.makeBlockWitness(bwb, rs, codeMap, bin)
	if err != nil {
		return nil, nil, err
	}

	var b bytes.Buffer

	stats, err := bwb.WriteTo(&b)

	if err != nil {
		return nil, nil, err
	}

	return b.Bytes(), NewBlockWitnessStats(tds.blockNr, uint64(b.Len()), stats), nil
}

func (tds *TrieDbState) makeBlockWitness(bwb *trie.BlockWitnessBuilder, rs *trie.ResolveSet, codeMap map[common.Hash][]byte, bin bool) error {
	tds.tMu.Lock()
	defer tds.tMu.Unlock()

	if bin {
		if err := bwb.MakeBlockWitnessBin(trie.HexToBin(tds.t), rs, codeMap); err != nil {
			return err
		}
	} else {
		if err := bwb.MakeBlockWitness(tds.t, rs, codeMap); err != nil {
			return err
		}
	}
	return nil
}

func (tsw *TrieStateWriter) CreateContract(address common.Address) error {
	addrHash, err := tsw.tds.HashAddress(address, true /*save*/)
	if err != nil {
		return err
	}
	tsw.tds.currentBuffer.created[addrHash] = struct{}{}
	incarnation, err := tsw.tds.nextIncarnation(addrHash)
	if err != nil {
		return err
	}
	if account, ok := tsw.tds.currentBuffer.accountUpdates[addrHash]; ok && account != nil {
		account.SetIncarnation(incarnation)
	}
	return nil
}

func (tds *TrieDbState) TriePruningDebugDump() string {
	return tds.tp.DebugDump()
}

func (tds *TrieDbState) getBlockNr() uint64 {
	return atomic.LoadUint64(&tds.blockNr)
}

func (tds *TrieDbState) setBlockNr(n uint64) {
	atomic.StoreUint64(&tds.blockNr, n)
}
