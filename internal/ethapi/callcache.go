// Copyright 2026 The go-ethereum Authors
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

package ethapi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rpc"
	"golang.org/x/sync/singleflight"
)

var (
	callCacheHits   = metrics.NewRegisteredCounter("rpc/callcache/hit", nil)    // answered from the cache
	callCacheMisses = metrics.NewRegisteredCounter("rpc/callcache/miss", nil)   // ran, and stored its result
	callCacheJoins  = metrics.NewRegisteredCounter("rpc/callcache/joined", nil) // waited for the same call already running
	callCacheSkips  = metrics.NewRegisteredCounter("rpc/callcache/skip", nil)   // can't be cached, ran as usual
)

// Tags of a stored result, its first byte: the rest is the call's return data,
// or the revert data of a call that reverted.
const (
	callResultOK     byte = 0
	callResultRevert byte = 1
)

// callKey identifies one call on one block: a hash of the block hash and every
// field of the call.
type callKey [sha256.Size]byte

// callCache keeps eth_call results, so a call asked again on the same block is
// answered from memory instead of running again. A result depends only on the
// block and the call, so it never goes stale: a new block has a new hash, and
// so new keys. Results of old blocks are no longer asked for, so they are the
// first to go once the cache is full.
//
// Identical calls arriving together run once: the others wait for that run
// and share its result. This matters right after a new block, when many
// clients ask the same calls before any result is stored.
type callCache struct {
	results  *lru.SizeConstrainedCache[callKey, []byte]
	inflight singleflight.Group
}

// newCallCache returns a cache holding up to the given megabytes of results, or
// nil, which turns caching off, if megabytes is 0 or less.
func newCallCache(megabytes int) *callCache {
	if megabytes <= 0 {
		return nil
	}
	maxBytes := uint64(megabytes) * 1024 * 1024
	return &callCache{results: lru.NewSizeConstrainedCache[callKey, []byte](maxBytes)}
}

// call answers an eth_call from the cache, or runs it and stores the result.
// ok is false if the call can't be cached and must run as usual: its block
// can't be resolved, it has fields the key doesn't cover, or the shared run it
// waited for failed in a way that may not be its own (a timeout, a client that
// went away). Overrides must be checked by the caller: calls with them are
// never cached.
func (c *callCache) call(ctx context.Context, b Backend, args TransactionArgs, blockNrOrHash rpc.BlockNumberOrHash) (ret hexutil.Bytes, ok bool, err error) {
	if !cacheableCall(args) {
		callCacheSkips.Inc(1)
		return nil, false, nil
	}
	// Resolve "latest" and the other tags to a block now, and run the call on
	// exactly that block, so a block arriving meanwhile can't mix two blocks'
	// results under one key. Errors are left to the usual path to report.
	header, herr := b.HeaderByNumberOrHash(ctx, blockNrOrHash)
	if herr != nil || header == nil {
		callCacheSkips.Inc(1)
		return nil, false, nil
	}
	key := makeCallKey(header.Hash(), args)
	if stored, found := c.results.Get(key); found {
		callCacheHits.Inc(1)
		ret, err = decodeCallResult(stored)
		return ret, true, err
	}
	pinned := rpc.BlockNumberOrHashWithHash(header.Hash(), blockNrOrHash.RequireCanonical)

	// led tells this request's own run apart from one it joined. The run sets
	// it before its result is sent, so it is safe to read once that arrives.
	var led bool
	results := c.inflight.DoChan(string(key[:]), func() (any, error) {
		led = true
		result, err := DoCall(ctx, b, args, pinned, nil, nil, b.RPCEVMTimeout(), b.RPCGasCap())
		if err != nil {
			return nil, err
		}
		switch {
		case result.Err == nil:
			c.results.Add(key, append([]byte{callResultOK}, result.Return()...))
		case errors.Is(result.Err, vm.ErrExecutionReverted):
			c.results.Add(key, append([]byte{callResultRevert}, result.Revert()...))
		}
		// Other EVM errors, such as running out of gas, aren't stored, but
		// they come from the call itself, so the waiting requests share them.
		ret, err := callReturn(result.Return(), result.Revert(), result.Err)
		return callOutcome{ret, err}, nil
	})
	select {
	case <-ctx.Done():
		return nil, true, ctx.Err()
	case res := <-results:
		switch {
		case led:
			callCacheMisses.Inc(1)
		case res.Err != nil:
			// The shared run failed, maybe for reasons of the request that
			// started it, so this one runs the call for itself.
			return nil, false, nil
		default:
			callCacheJoins.Inc(1)
		}
		if res.Err != nil {
			return nil, true, res.Err
		}
		outcome := res.Val.(callOutcome)
		return outcome.ret, true, outcome.err
	}
}

// callOutcome is what eth_call answers for a call that ran to its end.
type callOutcome struct {
	ret hexutil.Bytes
	err error
}

// callReturn turns a finished call into eth_call's answer, the same way
// BlockChainAPI.Call does.
func callReturn(ret, revert []byte, callErr error) (hexutil.Bytes, error) {
	if errors.Is(callErr, vm.ErrExecutionReverted) {
		return nil, newRevertError(revert)
	}
	return ret, callErr
}

// decodeCallResult turns a stored result back into eth_call's answer.
func decodeCallResult(stored []byte) (hexutil.Bytes, error) {
	if stored[0] == callResultRevert {
		return callReturn(nil, stored[1:], vm.ErrExecutionReverted)
	}
	return stored[1:], nil
}

// cacheableCall reports whether the key covers all of the call's fields. Blob
// and set-code fields are left out of the key, as eth_calls rarely carry them.
func cacheableCall(args TransactionArgs) bool {
	return len(args.BlobHashes) == 0 && len(args.Blobs) == 0 && len(args.Commitments) == 0 &&
		len(args.Proofs) == 0 && len(args.AuthorizationList) == 0
}

// makeCallKey hashes the block hash and every field of the call that
// cacheableCall lets through. Each optional field starts with a byte saying
// whether it is set, and each variable-length one with its length, so two
// different calls can't write the same bytes.
func makeCallKey(blockHash common.Hash, args TransactionArgs) callKey {
	h := sha256.New()
	h.Write(blockHash[:])
	writeAddress(h, args.From)
	writeAddress(h, args.To)
	writeUint64(h, (*uint64)(args.Gas))
	writeBig(h, args.GasPrice)
	writeBig(h, args.MaxFeePerGas)
	writeBig(h, args.MaxPriorityFeePerGas)
	writeBig(h, args.Value)
	writeUint64(h, (*uint64)(args.Nonce))
	writeBytes(h, args.Data)
	writeBytes(h, args.Input)
	writeBig(h, args.ChainID)
	writeBig(h, args.BlobFeeCap)

	if args.AccessList == nil {
		writeSet(h, false)
	} else {
		writeSet(h, true)
		writeLength(h, len(*args.AccessList))
		for _, tuple := range *args.AccessList {
			h.Write(tuple.Address[:])
			writeLength(h, len(tuple.StorageKeys))
			for _, slot := range tuple.StorageKeys {
				h.Write(slot[:])
			}
		}
	}
	var key callKey
	h.Sum(key[:0])
	return key
}

// writeSet writes whether an optional field is set.
func writeSet(h hash.Hash, set bool) {
	if set {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
}

// writeLength writes the length of a variable-length field.
func writeLength(h hash.Hash, n int) {
	h.Write(binary.BigEndian.AppendUint64(nil, uint64(n)))
}

func writeAddress(h hash.Hash, addr *common.Address) {
	writeSet(h, addr != nil)
	if addr != nil {
		h.Write(addr[:])
	}
}

func writeUint64(h hash.Hash, n *uint64) {
	writeSet(h, n != nil)
	if n != nil {
		h.Write(binary.BigEndian.AppendUint64(nil, *n))
	}
}

// writeBig writes a number's size and bytes. Numbers decoded from RPC are
// never negative, so the sign, which Bytes leaves out, is not needed.
func writeBig(h hash.Hash, n *hexutil.Big) {
	writeSet(h, n != nil)
	if n != nil {
		b := (*big.Int)(n).Bytes()
		writeLength(h, len(b))
		h.Write(b)
	}
}

func writeBytes(h hash.Hash, b *hexutil.Bytes) {
	writeSet(h, b != nil)
	if b != nil {
		writeLength(h, len(*b))
		h.Write(*b)
	}
}
