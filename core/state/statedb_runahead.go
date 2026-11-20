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
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// RunAheadCopy returns a copy of the state for a scout run: the same call,
// executed beside the real one only to find out which storage slots it reads.
// The copy's storage reads never wait on disk. A slot read for the first time
// is handed to a worker and reads as zero, so the scout carries on at once
// while the worker loads it. Reading the slot again
// gives its real value once the worker is done. That's how the scout finds the
// right path on slots it reads over and over, such as a proxy's implementation
// slot, without ever waiting. The reads fill the shared state caches beneath,
// so the real call then finds most of its slots in memory. Accounts and code
// are read normally, since the scout needs them to run the right code at all.
//
// Workers start as reads come in, up to the given number: a call that reads a
// slot or two starts one or two, while a big multicall gets them all.
//
// The scout's results are meaningless and must be thrown away. The copy shares
// nothing writable with s, so running it never changes the real call's result.
//
// The returned function stops the workers. It doesn't wait for a read in
// progress, so the real call's response isn't held up by the scout.
func (s *StateDB) RunAheadCopy(threads int) (scout *StateDB, stop func()) {
	scout = s.Copy()
	if threads <= 0 {
		return scout, func() {}
	}
	r := newRunAheadReader(s.reader, threads)
	scout.reader = newReader(s.reader, r)
	// The scout's state objects would otherwise keep the first value they were
	// given, a zero for a slot still loading, and never see the real one.
	scout.noStorageCache = true
	return scout, r.close
}

// SetStorageReadHook makes s call hook with the duration of every storage read
// that goes to its reader, so not reads it already holds in memory. nil removes
// the hook. The hook runs on the goroutine doing the read, in the middle of the
// read, so it must return at once. Copies of s don't inherit it.
func (s *StateDB) SetStorageReadHook(hook func(time.Duration)) {
	s.storageReadHook = hook
}

// runAheadSlot is one storage slot of one account.
type runAheadSlot struct {
	addr common.Address
	slot common.Hash
}

// runAheadReader is the scout's state reader. Account reads go straight to the
// real reader; storage reads go through the workers and never wait for them.
type runAheadReader struct {
	StateReader

	tasks   chan runAheadSlot
	term    chan struct{}
	once    sync.Once
	threads int          // most workers to start
	idle    atomic.Int32 // workers started and not reading a slot

	lock    sync.Mutex
	started int                          // workers started so far
	loaded  map[runAheadSlot]common.Hash // slots the workers have read, with their values
	asked   map[runAheadSlot]struct{}    // slots handed to the workers, loaded or not
}

// newRunAheadReader returns a reader that reads storage slots from reader on
// the scout's behalf, with up to the given number of workers.
func newRunAheadReader(reader StateReader, threads int) *runAheadReader {
	return &runAheadReader{
		StateReader: reader,
		// A few slots per worker, so the scout can queue reads while all
		// workers are busy, and only blocks once it is far ahead of them.
		tasks:   make(chan runAheadSlot, 4*threads),
		term:    make(chan struct{}),
		threads: threads,
		loaded:  make(map[runAheadSlot]common.Hash),
		asked:   make(map[runAheadSlot]struct{}),
	}
}

// work reads queued slots until the reader is closed. It is started idle.
func (r *runAheadReader) work() {
	for {
		select {
		case <-r.term:
			return
		case t := <-r.tasks:
			r.idle.Add(-1)
			// A failed read is left to the real call, which will meet the
			// same slot; the scout keeps reading it as zero.
			value, err := r.StateReader.Storage(t.addr, t.slot)
			if err == nil {
				r.lock.Lock()
				r.loaded[t] = value
				r.lock.Unlock()
			}
			r.idle.Add(1)
		}
	}
}

// Storage implements StateReader. It returns the slot's real value if a worker
// has loaded it, and zero otherwise, handing the slot to the workers the first
// time it is read. It never waits for a worker, only for room in the queue.
func (r *runAheadReader) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	t := runAheadSlot{addr: addr, slot: slot}

	r.lock.Lock()
	if value, ok := r.loaded[t]; ok {
		r.lock.Unlock()
		return value, nil
	}
	if _, ok := r.asked[t]; ok {
		r.lock.Unlock()
		return common.Hash{}, nil // still loading
	}
	r.asked[t] = struct{}{}
	// Start a worker only when none is free, so small calls start few.
	if r.idle.Load() == 0 && r.started < r.threads {
		r.started++
		r.idle.Add(1)
		go r.work()
	}
	r.lock.Unlock()

	select {
	case r.tasks <- t:
	case <-r.term:
	}
	return common.Hash{}, nil
}

// close stops the workers. Slots still queued are dropped.
func (r *runAheadReader) close() {
	r.once.Do(func() { close(r.term) })
}
