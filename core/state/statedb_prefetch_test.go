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

package state

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/testrand"
)

// TestPrefetchAccessList checks that every account and slot of an access list is
// read exactly once through the state's reader, including an address listed twice.
func TestPrefetchAccessList(t *testing.T) {
	var (
		counter = newRefStateReader()
		addrA   = testrand.Address()
		addrB   = testrand.Address()
		list    = types.AccessList{
			{Address: addrA, StorageKeys: []common.Hash{testrand.Hash(), testrand.Hash()}},
			{Address: addrB, StorageKeys: []common.Hash{testrand.Hash()}},
			{Address: addrA, StorageKeys: []common.Hash{testrand.Hash()}},
		}
		// Two accounts, plus four slots.
		total = 6
	)
	sdb, err := NewWithReader(types.EmptyRootHash, NewDatabaseForTesting(), newReader(nil, counter))
	if err != nil {
		t.Fatal(err)
	}
	stop := sdb.PrefetchAccessList(list, 4)

	// The reads run in the background, so wait for them before stopping.
	done := func() bool {
		counter.lock.Lock()
		defer counter.lock.Unlock()
		return counter.validate(total) == nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for !done() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()
	counter.lock.Lock()
	defer counter.lock.Unlock()
	if err := counter.validate(total); err != nil {
		t.Fatal(err)
	}
}

// TestPrefetchAccessListNothingToDo checks that an empty list or no workers starts
// nothing and that its stop function is still safe to call.
func TestPrefetchAccessListNothingToDo(t *testing.T) {
	counter := newRefStateReader()
	sdb, err := NewWithReader(types.EmptyRootHash, NewDatabaseForTesting(), newReader(nil, counter))
	if err != nil {
		t.Fatal(err)
	}
	list := types.AccessList{{Address: testrand.Address(), StorageKeys: []common.Hash{testrand.Hash()}}}

	sdb.PrefetchAccessList(nil, 4)()
	sdb.PrefetchAccessList(list, 0)()
	if len(counter.accounts) != 0 || len(counter.storages) != 0 {
		t.Fatalf("got %d accounts and %d storage reads, want none", len(counter.accounts), len(counter.storages))
	}
}
