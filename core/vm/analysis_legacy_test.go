// Copyright 2017 The go-ethereum Authors
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

package vm

import (
	"math/bits"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func TestJumpDestAnalysis(t *testing.T) {
	tests := []struct {
		code  []byte
		exp   byte
		which int
	}{
		{[]byte{byte(PUSH1), 0x01, 0x01, 0x01}, 0b0000_0010, 0},
		{[]byte{byte(PUSH1), byte(PUSH1), byte(PUSH1), byte(PUSH1)}, 0b0000_1010, 0},
		{[]byte{0x00, byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(PUSH1)}, 0b0101_0100, 0},
		{[]byte{byte(PUSH8), byte(PUSH8), byte(PUSH8), byte(PUSH8), byte(PUSH8), byte(PUSH8), byte(PUSH8), byte(PUSH8), 0x01, 0x01, 0x01}, bits.Reverse8(0x7F), 0},
		{[]byte{byte(PUSH8), 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01}, 0b0000_0001, 1},
		{[]byte{0x01, 0x01, 0x01, 0x01, 0x01, byte(PUSH2), byte(PUSH2), byte(PUSH2), 0x01, 0x01, 0x01}, 0b1100_0000, 0},
		{[]byte{0x01, 0x01, 0x01, 0x01, 0x01, byte(PUSH2), 0x01, 0x01, 0x01, 0x01, 0x01}, 0b0000_0000, 1},
		{[]byte{byte(PUSH3), 0x01, 0x01, 0x01, byte(PUSH1), 0x01, 0x01, 0x01, 0x01, 0x01, 0x01}, 0b0010_1110, 0},
		{[]byte{byte(PUSH3), 0x01, 0x01, 0x01, byte(PUSH1), 0x01, 0x01, 0x01, 0x01, 0x01, 0x01}, 0b0000_0000, 1},
		{[]byte{0x01, byte(PUSH8), 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01}, 0b1111_1100, 0},
		{[]byte{0x01, byte(PUSH8), 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01}, 0b0000_0011, 1},
		{[]byte{byte(PUSH16), 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01}, 0b1111_1110, 0},
		{[]byte{byte(PUSH16), 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01}, 0b1111_1111, 1},
		{[]byte{byte(PUSH16), 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01}, 0b0000_0001, 2},
		{[]byte{byte(PUSH8), 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, byte(PUSH1), 0x01}, 0b1111_1110, 0},
		{[]byte{byte(PUSH8), 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, byte(PUSH1), 0x01}, 0b0000_0101, 1},
		{[]byte{byte(PUSH32)}, 0b1111_1110, 0},
		{[]byte{byte(PUSH32)}, 0b1111_1111, 1},
		{[]byte{byte(PUSH32)}, 0b1111_1111, 2},
		{[]byte{byte(PUSH32)}, 0b1111_1111, 3},
		{[]byte{byte(PUSH32)}, 0b0000_0001, 4},
	}
	for i, test := range tests {
		ret := codeBitmap(test.code)
		if ret[test.which] != test.exp {
			t.Fatalf("test %d: expected %x, got %02x", i, test.exp, ret[test.which])
		}
	}
}

const analysisCodeSize = 1200 * 1024

func BenchmarkJumpdestAnalysis_1200k(bench *testing.B) {
	// 1.4 ms
	code := make([]byte, analysisCodeSize)
	bench.SetBytes(analysisCodeSize)
	for bench.Loop() {
		codeBitmap(code)
	}
}
func BenchmarkJumpdestHashing_1200k(bench *testing.B) {
	// 4 ms
	code := make([]byte, analysisCodeSize)
	bench.SetBytes(analysisCodeSize)
	for bench.Loop() {
		crypto.Keccak256Hash(code)
	}
}

func BenchmarkJumpdestOpAnalysis(bench *testing.B) {
	var op OpCode
	bencher := func(b *testing.B) {
		code := make([]byte, analysisCodeSize)
		b.SetBytes(analysisCodeSize)
		for i := range code {
			code[i] = byte(op)
		}
		bits := make(BitVec, len(code)/8+1+4)
		for b.Loop() {
			clear(bits)
			codeBitmapInternal(code, bits)
		}
	}
	for op = PUSH1; op <= PUSH32; op++ {
		bench.Run(op.String(), bencher)
	}
	op = JUMPDEST
	bench.Run(op.String(), bencher)
	op = STOP
	bench.Run(op.String(), bencher)
}

// A bitmap found in the shared cache must also be saved in the EVM's own map, so
// that later calls to the same code don't take the shared cache's lock again.
func TestIsCodeSavesSharedCacheHit(t *testing.T) {
	// Three bytes of code. Byte 0 is PUSH1, so byte 1 is the data it pushes, even
	// though its value looks like a JUMPDEST. Byte 2 is a real JUMPDEST instruction.
	code := []byte{byte(PUSH1), byte(JUMPDEST), byte(JUMPDEST)}
	hash := crypto.Keccak256Hash(code)

	// Put this code's bitmap in the shared cache, so that the lookup below finds it.
	codeBitmapCache.Add(hash, codeBitmap(code))

	// Start with an empty EVM map, as at the start of a request.
	jumpDests := newMapJumpDests()
	contract := &Contract{Code: code, CodeHash: hash, jumpDests: jumpDests}

	// The bitmap from the shared cache must still give the right answers. Byte 1
	// looks like a JUMPDEST but is PUSH1's data, so it must not count as code, or
	// a contract could jump into its own data. As the first isCode call, this also
	// runs the lookup that fills the EVM's map, which the last check relies on.
	if contract.isCode(1) {
		t.Error("PUSH1's data was reported as code")
	}
	if !contract.isCode(2) {
		t.Error("the instruction after PUSH1's data wasn't reported as code")
	}

	// This is what the fix adds: the lookup also filled the EVM's own map.
	if _, ok := jumpDests.Load(hash); !ok {
		t.Error("the shared cache's bitmap wasn't saved in the EVM's own map")
	}

	// A later call to this code is a new contract sharing the EVM's map. Remove the
	// bitmap from the shared cache: a call that went back there would rebuild it and
	// add it again.
	codeBitmapCache.Remove(hash)
	next := &Contract{Code: code, CodeHash: hash, jumpDests: jumpDests}
	if !next.isCode(2) {
		t.Error("the next call got the wrong answer")
	}
	if codeBitmapCache.Contains(hash) {
		t.Error("the next call went back to the shared cache")
	}
}
