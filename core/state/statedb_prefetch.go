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
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// PrefetchAccessList reads the accounts and storage slots named in list in the
// background, spread over the given number of workers. The reads go through the
// state's own reader, so they fill the shared state caches beneath it, and a call
// running on this state then finds those slots in memory instead of waiting on
// disk for each one in turn. It doesn't change the state's contents.
//
// The returned function stops the workers and waits for them to exit. Call it
// once the call has finished.
func (s *StateDB) PrefetchAccessList(list types.AccessList, threads int) (stop func()) {
	if len(list) == 0 || threads <= 0 {
		return func() {}
	}
	// An address may appear more than once in a list, so merge its slots.
	slots := make(map[common.Address][]common.Hash, len(list))
	for _, entry := range list {
		slots[entry.Address] = append(slots[entry.Address], entry.StorageKeys...)
	}
	return newPrefetchStateReader(s.reader, slots, threads).Close
}
