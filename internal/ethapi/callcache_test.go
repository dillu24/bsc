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
	"bytes"
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/ethapi/override"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// newCallCacheTestAPI returns an API with the cache on, over a chain of 10
// blocks with two contracts: one returns the block number, so results differ
// per block, and one always reverts with 42.
func newCallCacheTestAPI(t *testing.T) (api *BlockChainAPI, numberAddr, revertAddr common.Address) {
	numberAddr = common.HexToAddress("0x1111")
	revertAddr = common.HexToAddress("0x2222")
	genesis := &core.Genesis{
		Config: params.MergedTestChainConfig,
		Alloc: types.GenesisAlloc{
			// NUMBER, then return it as one word.
			numberAddr: {Code: common.FromHex("0x4360005260206000f3")},
			// Revert with 42 as one word.
			revertAddr: {Code: common.FromHex("0x602a60005260206000fd")},
		},
	}
	backend := newTestBackend(t, 10, genesis, beacon.New(ethash.NewFaker()), func(i int, b *core.BlockGen) {
		b.SetPoS()
	})
	api = NewBlockChainAPI(backend)
	api.calls = newCallCache(1)
	return api, numberAddr, revertAddr
}

// word returns n as a 32-byte return value.
func word(n int64) hexutil.Bytes {
	return common.BigToHash(big.NewInt(n)).Bytes()
}

func blockTag(n rpc.BlockNumber) *rpc.BlockNumberOrHash {
	tag := rpc.BlockNumberOrHashWithNumber(n)
	return &tag
}

// TestCallCacheResults checks that cached answers match what the call returns,
// per block, and that reverts come back the same way.
func TestCallCacheResults(t *testing.T) {
	api, numberAddr, revertAddr := newCallCacheTestAPI(t)
	ctx := context.Background()

	// The same call on different blocks must not share a result.
	for range 2 {
		for _, n := range []rpc.BlockNumber{5, 6, rpc.LatestBlockNumber} {
			want := int64(n)
			if n == rpc.LatestBlockNumber {
				want = 10
			}
			got, err := api.Call(ctx, TransactionArgs{To: &numberAddr}, blockTag(n), nil, nil)
			if err != nil {
				t.Fatalf("block %d: %v", n, err)
			}
			if !bytes.Equal(got, word(want)) {
				t.Fatalf("block %d: got %x, want %x", n, got, word(want))
			}
		}
	}
	// A revert is stored and answered again as the same revert.
	for i := range 2 {
		_, err := api.Call(ctx, TransactionArgs{To: &revertAddr}, blockTag(rpc.LatestBlockNumber), nil, nil)
		var revert *revertError
		if !errors.As(err, &revert) {
			t.Fatalf("call %d: got %v, want a revert", i, err)
		}
		if revert.reason != hexutil.Encode(word(42)) {
			t.Fatalf("call %d: revert data %s, want %x", i, revert.reason, word(42))
		}
	}
}

// TestCallCacheIsUsed checks that a stored result answers the call, and that
// calls with overrides skip the cache.
func TestCallCacheIsUsed(t *testing.T) {
	api, numberAddr, _ := newCallCacheTestAPI(t)
	ctx := context.Background()
	args := TransactionArgs{To: &numberAddr}

	if _, err := api.Call(ctx, args, blockTag(rpc.LatestBlockNumber), nil, nil); err != nil {
		t.Fatal(err)
	}
	key := makeCallKey(api.b.CurrentHeader().Hash(), args)
	if _, ok := api.calls.results.Get(key); !ok {
		t.Fatal("result was not stored")
	}
	// Store a result the call can't return, in a fresh cache, as Add never
	// replaces a stored result. The call must answer with it.
	api.calls = newCallCache(1)
	api.calls.results.Add(key, append([]byte{callResultOK}, 7))

	got, err := api.Call(ctx, args, blockTag(rpc.LatestBlockNumber), nil, nil)
	if err != nil || !bytes.Equal(got, []byte{7}) {
		t.Fatalf("got %x, %v; want the stored 07", got, err)
	}
	// An override changes what the call sees, so it must run.
	empty := hexutil.Bytes{}
	overrides := override.StateOverride{numberAddr: {Code: &empty}}
	got, err = api.Call(ctx, args, blockTag(rpc.LatestBlockNumber), &overrides, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("with override: got %x, %v; want empty", got, err)
	}
}

// TestCallCacheConcurrent sends the same call from many goroutines at once.
// They all must get the right answer, whether they ran it, waited for it or
// read it from the cache.
func TestCallCacheConcurrent(t *testing.T) {
	api, numberAddr, _ := newCallCacheTestAPI(t)

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := api.Call(context.Background(), TransactionArgs{To: &numberAddr}, blockTag(7), nil, nil)
			if err != nil || !bytes.Equal(got, word(7)) {
				t.Errorf("got %x, %v; want %x", got, err, word(7))
			}
		}()
	}
	wg.Wait()
}

// TestCallKeyFields checks that calls differing in any one field get different
// keys, including set-but-empty against unset.
func TestCallKeyFields(t *testing.T) {
	addr := common.HexToAddress("0x1234")
	data := hexutil.Bytes{1, 2}
	empty := hexutil.Bytes{}
	gas := hexutil.Uint64(21000)
	one := (*hexutil.Big)(big.NewInt(1))

	calls := map[string]TransactionArgs{
		"none":           {},
		"from":           {From: &addr},
		"to":             {To: &addr},
		"gas":            {Gas: &gas},
		"nonce":          {Nonce: &gas},
		"gasPrice":       {GasPrice: one},
		"value":          {Value: one},
		"data":           {Data: &data},
		"input":          {Input: &data},
		"empty data":     {Data: &empty},
		"chainId":        {ChainID: one},
		"empty list":     {AccessList: &types.AccessList{}},
		"list":           {AccessList: &types.AccessList{{Address: addr}}},
		"list with slot": {AccessList: &types.AccessList{{Address: addr, StorageKeys: []common.Hash{{}}}}},
	}
	seen := make(map[callKey]string)
	for name, args := range calls {
		key := makeCallKey(common.Hash{}, args)
		if other, ok := seen[key]; ok {
			t.Errorf("%s and %s have the same key", name, other)
		}
		seen[key] = name
	}
	if makeCallKey(common.Hash{1}, TransactionArgs{}) == makeCallKey(common.Hash{2}, TransactionArgs{}) {
		t.Error("different blocks have the same key")
	}
}
