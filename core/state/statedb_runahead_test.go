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
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/testrand"
)

// slowStateReader serves fixed storage values, taking delay for the slots listed
// in slow, and counts how often each slot was read.
type slowStateReader struct {
	values map[common.Hash]common.Hash
	slow   map[common.Hash]bool
	delay  time.Duration

	lock  sync.Mutex
	reads map[common.Hash]int
}

func newSlowStateReader(values map[common.Hash]common.Hash, slow map[common.Hash]bool, delay time.Duration) *slowStateReader {
	return &slowStateReader{values: values, slow: slow, delay: delay, reads: make(map[common.Hash]int)}
}

func (r *slowStateReader) Account(addr common.Address) (*types.StateAccount, error) {
	return types.NewEmptyStateAccount(), nil
}

func (r *slowStateReader) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	if r.slow[slot] {
		time.Sleep(r.delay)
	}
	r.lock.Lock()
	r.reads[slot]++
	r.lock.Unlock()
	return r.values[slot], nil
}

func (r *slowStateReader) readCount(slot common.Hash) int {
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.reads[slot]
}

// eventually polls cond until it holds or a few seconds have passed.
func eventually(cond func() bool) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// TestRunAheadReader checks that a slot reads as zero without waiting the first
// time, and with its real value once a worker has loaded it.
func TestRunAheadReader(t *testing.T) {
	var (
		addr = testrand.Address()
		slot = testrand.Hash()
		want = testrand.Hash()
		base = newSlowStateReader(map[common.Hash]common.Hash{slot: want}, map[common.Hash]bool{slot: true}, 200*time.Millisecond)
	)
	r := newRunAheadReader(base, 2)
	defer r.close()

	start := time.Now()
	if got, _ := r.Storage(addr, slot); got != (common.Hash{}) {
		t.Fatalf("first read: got %x, want zero", got)
	}
	if waited := time.Since(start); waited >= base.delay {
		t.Fatalf("scout waited %v for the slot, want less than %v", waited, base.delay)
	}
	if !eventually(func() bool { got, _ := r.Storage(addr, slot); return got == want }) {
		t.Fatal("slot never read with its real value")
	}
}

// TestRunAheadReaderAsksOnce checks that reading a slot again while it is still
// loading doesn't send it to the workers a second time.
func TestRunAheadReaderAsksOnce(t *testing.T) {
	var (
		addr = testrand.Address()
		slot = testrand.Hash()
		base = newSlowStateReader(map[common.Hash]common.Hash{slot: testrand.Hash()}, map[common.Hash]bool{slot: true}, 50*time.Millisecond)
	)
	r := newRunAheadReader(base, 4)
	defer r.close()

	for i := 0; i < 10; i++ {
		r.Storage(addr, slot)
	}
	if !eventually(func() bool { return base.readCount(slot) > 0 }) {
		t.Fatal("slot was never read")
	}
	time.Sleep(2 * base.delay) // give any duplicate read time to show up
	if n := base.readCount(slot); n != 1 {
		t.Fatalf("slot read %d times, want 1", n)
	}
}

// TestRunAheadReaderStartsWorkersOnDemand checks that workers start only as
// reads need them: none before the first read, one for a single slot, and never
// more than the limit however many slots pile up.
func TestRunAheadReaderStartsWorkersOnDemand(t *testing.T) {
	var (
		addr  = testrand.Address()
		slots = make([]common.Hash, 50)
		slow  = make(map[common.Hash]bool)
	)
	for i := range slots {
		slots[i] = testrand.Hash()
		slow[slots[i]] = true
	}
	base := newSlowStateReader(nil, slow, 20*time.Millisecond)
	r := newRunAheadReader(base, 4)
	defer r.close()

	started := func() int {
		r.lock.Lock()
		defer r.lock.Unlock()
		return r.started
	}
	if n := started(); n != 0 {
		t.Fatalf("%d workers before any read, want 0", n)
	}
	r.Storage(addr, slots[0])
	if n := started(); n != 1 {
		t.Fatalf("%d workers after one read, want 1", n)
	}
	// Slow reads keep the workers busy, so the rest start up to the limit.
	for _, slot := range slots[1:] {
		r.Storage(addr, slot)
	}
	if n := started(); n != 4 {
		t.Fatalf("%d workers with reads piled up, want the limit of 4", n)
	}
}

// TestRunAheadReaderClosed checks that a closed reader returns zero at once
// instead of blocking the scout.
func TestRunAheadReaderClosed(t *testing.T) {
	r := newRunAheadReader(newSlowStateReader(nil, nil, 0), 1)
	r.close()
	r.close() // safe to call twice

	if got, _ := r.Storage(testrand.Address(), testrand.Hash()); got != (common.Hash{}) {
		t.Fatalf("got %x, want zero", got)
	}
}

// TestRunAheadCopy checks that only the scout reads through the run-ahead reader
// and skips the storage cache, so the real call never sees a zero the scout was
// given and keeps caching as usual.
func TestRunAheadCopy(t *testing.T) {
	base := newSlowStateReader(nil, nil, 0)
	sdb, err := NewWithReader(types.EmptyRootHash, NewDatabaseForTesting(), newReader(nil, base))
	if err != nil {
		t.Fatal(err)
	}
	scout, stop := sdb.RunAheadCopy(2)
	defer stop()

	if _, ok := scout.reader.(*reader).StateReader.(*runAheadReader); !ok {
		t.Fatalf("scout reads through %T, want *runAheadReader", scout.reader.(*reader).StateReader)
	}
	if !scout.noStorageCache {
		t.Fatal("scout keeps the storage values it reads")
	}
	if sdb.reader.(*reader).StateReader != base {
		t.Fatal("original state no longer reads through its own reader")
	}
	if sdb.noStorageCache {
		t.Fatal("original state stopped caching storage")
	}
	// Without workers there is nothing to read ahead with, so the copy is plain.
	plain, stopPlain := sdb.RunAheadCopy(0)
	stopPlain()
	if plain.reader != sdb.reader || plain.noStorageCache {
		t.Fatal("copy without workers should be a plain copy")
	}
}

// TestRunAheadScoutSeesLoadedValue checks the whole scout state: a slot reads as
// zero while loading and with its real value afterwards, rather than the state
// object holding on to the first zero.
func TestRunAheadScoutSeesLoadedValue(t *testing.T) {
	var (
		addr = testrand.Address()
		slot = testrand.Hash()
		want = testrand.Hash()
		base = newSlowStateReader(map[common.Hash]common.Hash{slot: want}, map[common.Hash]bool{slot: true}, 50*time.Millisecond)
	)
	sdb, err := NewWithReader(types.EmptyRootHash, NewDatabaseForTesting(), newReader(nil, base))
	if err != nil {
		t.Fatal(err)
	}
	scout, stop := sdb.RunAheadCopy(2)
	defer stop()

	if got := scout.GetState(addr, slot); got != (common.Hash{}) {
		t.Fatalf("first read: got %x, want zero", got)
	}
	if !eventually(func() bool { return scout.GetState(addr, slot) == want }) {
		t.Fatal("scout never saw the loaded value")
	}
	// The real state reads the slot itself and is unaffected by the scout.
	if got := sdb.GetState(addr, slot); got != want {
		t.Fatalf("real state: got %x, want %x", got, want)
	}
}

// TestStorageReadHook checks that the hook hears about reads that go to the
// reader, not about slots the state already holds, and that a copy, such as a
// scout, doesn't inherit it.
func TestStorageReadHook(t *testing.T) {
	var (
		addr = testrand.Address()
		slot = testrand.Hash()
		base = newSlowStateReader(map[common.Hash]common.Hash{slot: testrand.Hash()}, map[common.Hash]bool{slot: true}, 5*time.Millisecond)
	)
	sdb, err := NewWithReader(types.EmptyRootHash, NewDatabaseForTesting(), newReader(nil, base))
	if err != nil {
		t.Fatal(err)
	}
	var reads []time.Duration
	sdb.SetStorageReadHook(func(d time.Duration) { reads = append(reads, d) })

	if cpy := sdb.Copy(); cpy.storageReadHook != nil {
		t.Fatal("copy inherited the storage read hook")
	}
	sdb.GetState(addr, slot)
	sdb.GetState(addr, slot) // held in memory now, so not a read
	if len(reads) != 1 {
		t.Fatalf("hook heard %d reads, want 1", len(reads))
	}
	if reads[0] < base.delay {
		t.Fatalf("hook heard %v, want at least the reader's %v", reads[0], base.delay)
	}
	sdb.SetStorageReadHook(nil)
	sdb.GetState(addr, testrand.Hash())
	if len(reads) != 1 {
		t.Fatal("removed hook still called")
	}
}
