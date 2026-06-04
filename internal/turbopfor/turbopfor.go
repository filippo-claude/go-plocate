// go-plocate: a read-only, pure-Go reimplementation of the plocate database
// reader. This is a derivative work of plocate by Steinar H. Gunderson
// (https://git.sesse.net/plocate); the Go port was produced by an LLM agent.
//
// Copyright 2020 Steinar H. Gunderson (original plocate, C++).
// Copyright 2026 Filippo Valsorda (Go port; written by an LLM agent).
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 2 of the License, or (at your option)
// any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS
// FOR A PARTICULAR PURPOSE. See the GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License along with
// this program (see the file COPYING). If not, see
// <https://www.gnu.org/licenses/>.

// Package turbopfor decodes the subset of the TurboPFor integer-compression
// format that plocate uses for its posting lists.
//
// This is a port of the scalar (non-SIMD) decode path of plocate's
// turbopfor.cpp, specifically the single entry point that plocate relies on:
// decode_pfor_delta1_128 with interleaved=true. As in the original, only the
// "delta plus 1" codec with 128-value blocks and 32-bit document IDs is
// implemented; the document IDs are strictly increasing, so each stored value
// is the gap to the previous one minus one.
//
// The format is documented inline below and in Michael Stapelberg's analysis
// of TurboPFor (https://michael.stapelberg.ch/posts/2019-02-05-turbopfor-analysis/).
// Stapelberg's own Go decoder (github.com/stapelberg/goturbopfor) targets a
// different variant (256-value blocks, no delta), so it cannot be used here;
// it was consulted only to cross-check understanding of the byte layout.
//
// Like the reference implementation, the decoder is NOT robust against
// malicious or corrupted input. Decode requires that its input slice has at
// least 16 bytes of readable slop past the encoded data; callers must arrange
// for this (see Decode).
package turbopfor

import (
	"encoding/binary"
	"math/bits"
)

// blockSize is the number of values in a full (interleaved) block.
const blockSize = 128

// Slop is the number of bytes past the logical end of the encoded data that
// the decoder may read. Callers must guarantee the input slice can be indexed
// this far past the encoded length.
const Slop = 16

// blockType is stored in the top two bits of a block's first byte.
const (
	blockFOR        = 0 // FOR (bit-packed, no exceptions)
	blockPForVB     = 1 // PFor with variable-byte exceptions
	blockPForBitmap = 2 // PFor with bitmap exceptions
	blockConstant   = 3 // all-equal deltas
)

func maskForBits(bitWidth uint) uint32 {
	if bitWidth >= 32 {
		return 0xFFFFFFFF
	}
	return (uint32(1) << bitWidth) - 1
}

func divRoundUp(val, div uint) uint {
	return (val + div - 1) / div
}

func bytesForPackedBits(num, bitWidth uint) uint {
	return divRoundUp(num*bitWidth, 8)
}

// readLE32 reads a little-endian uint32, reading up to 4 bytes past in[0].
func readLE32(in []byte) uint32 {
	return binary.LittleEndian.Uint32(in)
}

// readBaseval reads the first value of a posting list, using an encoding that
// resembles PrefixVarint. Returns the value and the number of bytes consumed.
func readBaseval(in []byte) (uint32, int) {
	switch {
	case in[0] < 128:
		return uint32(in[0]), 1
	case in[0] < 192:
		return (uint32(in[0])<<8 | uint32(in[1])) & 0x3fff, 2
	case in[0] < 224:
		return (uint32(in[0])<<16 | uint32(in[2])<<8 | uint32(in[1])) & 0x1fffff, 3
	case in[0] < 240:
		return (uint32(in[0])<<24 | uint32(in[1])<<16 | uint32(in[2])<<8 | uint32(in[3])) & 0xfffffff, 4
	default:
		// The reference implementation does not handle this case.
		panic("turbopfor: baseval >= 2^28 not implemented")
	}
}

// readVB reads a single variable-byte exception value. Does not read past the
// end of the encoded value. Returns the value and the number of bytes consumed.
func readVB(in []byte) (uint32, int) {
	switch {
	case in[0] <= 176:
		return uint32(in[0]), 1
	case in[0] <= 240:
		return (uint32(in[0]-177)<<8 | uint32(in[1])) + 177, 2
	case in[0] <= 248:
		return (uint32(in[0]-241)<<16 | uint32(binary.LittleEndian.Uint16(in[1:]))) + 16561, 3
	case in[0] == 249:
		return uint32(in[1]) | uint32(in[2])<<8 | uint32(in[3])<<16, 4
	case in[0] == 250:
		return readLE32(in[1:]), 5
	default:
		panic("turbopfor: invalid varbyte marker")
	}
}

// bitReader reads consecutive bitWidth-wide little-endian packed values.
// It may read up to 4 bytes past its current position when bitWidth is 0.
type bitReader struct {
	in       []byte
	pos      int
	bits     uint
	mask     uint32
	bitsUsed uint
}

func newBitReader(in []byte, bitWidth uint) bitReader {
	return bitReader{in: in, bits: bitWidth, mask: maskForBits(bitWidth)}
}

func (b *bitReader) read() uint32 {
	val := (readLE32(b.in[b.pos:]) >> b.bitsUsed) & b.mask
	b.bitsUsed += b.bits
	b.pos += int(b.bitsUsed / 8)
	b.bitsUsed %= 8
	return val
}

// interleavedBitReader reads packed values from one of four interleaved
// 32-bit streams (stride = 4*4 = 16 bytes). It may read up to 16 bytes past
// its current position.
type interleavedBitReader struct {
	in       []byte
	pos      int
	bits     uint
	mask     uint32
	bitsUsed uint
}

const interleavedStride = 4 * 4 // NumStreams(4) * sizeof(uint32)

func newInterleavedBitReader(in []byte, bitWidth uint) interleavedBitReader {
	return interleavedBitReader{in: in, bits: bitWidth, mask: maskForBits(bitWidth)}
}

func (b *interleavedBitReader) read() uint32 {
	var val uint32
	if b.bitsUsed+b.bits > 32 {
		val = (readLE32(b.in[b.pos:]) >> b.bitsUsed) |
			(readLE32(b.in[b.pos+interleavedStride:]) << (32 - b.bitsUsed))
	} else {
		val = readLE32(b.in[b.pos:]) >> b.bitsUsed
	}
	b.bitsUsed += b.bits
	b.pos += interleavedStride * int(b.bitsUsed/32)
	b.bitsUsed %= 32
	return val & b.mask
}

// Each decode* function decodes one block of num values into out[base:base+num],
// reading out[base-1] as the running previous value, and returns the number of
// input bytes consumed.

// decodeConstant handles a block whose deltas are all identical.
//
// Layout: bit width (6 bits) | type<<6, then the single base value (bitWidth
// bits, rounded up to a byte).
func decodeConstant(in []byte, num uint, out []uint32, base int) int {
	bitWidth := uint(in[0] & 0x3f)
	val := readLE32(in[1:])
	if bitWidth < 32 {
		val &= maskForBits(bitWidth)
	}
	prev := out[base-1]
	for i := uint(0); i < num; i++ {
		prev = val + prev + 1
		out[base+int(i)] = prev
	}
	return 1 + int(divRoundUp(bitWidth, 8))
}

// decodeFOR handles a bit-packed block without exceptions (partial blocks).
//
// Layout: bit width (6 bits) | type<<6, then num values of bitWidth bits.
func decodeFOR(in []byte, num uint, out []uint32, base int) int {
	bitWidth := uint(in[0] & 0x3f)
	prev := out[base-1]
	bs := newBitReader(in[1:], bitWidth)
	for i := uint(0); i < num; i++ {
		prev = bs.read() + prev + 1
		out[base+int(i)] = prev
	}
	return 1 + int(bytesForPackedBits(num, bitWidth))
}

// decodeFORInterleaved handles a full bit-packed block stored as four
// interleaved streams.
func decodeFORInterleaved(in []byte, out []uint32, base int) int {
	bitWidth := uint(in[0] & 0x3f)
	body := in[1:]
	bs0 := newInterleavedBitReader(body[0*4:], bitWidth)
	bs1 := newInterleavedBitReader(body[1*4:], bitWidth)
	bs2 := newInterleavedBitReader(body[2*4:], bitWidth)
	bs3 := newInterleavedBitReader(body[3*4:], bitWidth)
	for i := 0; i < blockSize/4; i++ {
		out[base+i*4+0] = bs0.read()
		out[base+i*4+1] = bs1.read()
		out[base+i*4+2] = bs2.read()
		out[base+i*4+3] = bs3.read()
	}
	prev := out[base-1]
	for i := 0; i < blockSize; i++ {
		prev = out[base+i] + prev + 1
		out[base+i] = prev
	}
	return 1 + int(bytesForPackedBits(blockSize, bitWidth))
}

// decodePForBitmapExceptions decodes the bitmap-exception preamble shared by
// the bitmap PFor blocks, writing the high bits of exceptional values into out.
// Returns the number of input bytes consumed.
//
// Layout: exception bit width (8 bits), bitmap of which values are exceptions
// (num bits, rounded up to a byte), then the exception values (num_exc values
// of exception-bit-width bits, rounded up to a byte).
func decodePForBitmapExceptions(in []byte, num uint, out []uint32, base int) int {
	exceptionBitWidth := uint(in[0])
	pos := 1
	bitmap := in[pos:]
	pos += int(divRoundUp(num, 8))

	numExceptions := uint(0)
	bs := newBitReader(in[pos:], exceptionBitWidth)
	for i := uint(0); i < num; i += 64 {
		exceptions := binary.LittleEndian.Uint64(bitmap[(i/64)*8:])
		if num-i < 64 {
			exceptions &= (uint64(1) << (num - i)) - 1
		}
		for exceptions != 0 {
			idx := uint(bits.TrailingZeros64(exceptions)) + i
			out[base+int(idx)] = bs.read()
			exceptions &= exceptions - 1
			numExceptions++
		}
	}
	pos += int(bytesForPackedBits(numExceptions, exceptionBitWidth))
	return pos
}

// decodePForBitmap handles a PFor block with bitmap exceptions (partial blocks).
func decodePForBitmap(in []byte, num uint, out []uint32, base int) int {
	for i := uint(0); i < num; i++ {
		out[base+int(i)] = 0
	}
	bitWidth := uint(in[0] & 0x3f)
	pos := 1
	pos += decodePForBitmapExceptions(in[pos:], num, out, base)

	prev := out[base-1]
	bs := newBitReader(in[pos:], bitWidth)
	for i := uint(0); i < num; i++ {
		prev = ((out[base+int(i)] << bitWidth) | bs.read()) + prev + 1
		out[base+int(i)] = prev
	}
	return pos + int(bytesForPackedBits(num, bitWidth))
}

// decodePForBitmapInterleaved handles a full PFor-bitmap block with the base
// values stored as four interleaved streams.
func decodePForBitmapInterleaved(in []byte, out []uint32, base int) int {
	for i := 0; i < blockSize; i++ {
		out[base+i] = 0
	}
	bitWidth := uint(in[0] & 0x3f)
	pos := 1
	pos += decodePForBitmapExceptions(in[pos:], blockSize, out, base)

	body := in[pos:]
	bs0 := newInterleavedBitReader(body[0*4:], bitWidth)
	bs1 := newInterleavedBitReader(body[1*4:], bitWidth)
	bs2 := newInterleavedBitReader(body[2*4:], bitWidth)
	bs3 := newInterleavedBitReader(body[3*4:], bitWidth)
	for i := 0; i < blockSize/4; i++ {
		out[base+i*4+0] = bs0.read() | (out[base+i*4+0] << bitWidth)
		out[base+i*4+1] = bs1.read() | (out[base+i*4+1] << bitWidth)
		out[base+i*4+2] = bs2.read() | (out[base+i*4+2] << bitWidth)
		out[base+i*4+3] = bs3.read() | (out[base+i*4+3] << bitWidth)
	}
	prev := out[base-1]
	for i := 0; i < blockSize; i++ {
		prev = out[base+i] + prev + 1
		out[base+i] = prev
	}
	return pos + int(bytesForPackedBits(blockSize, bitWidth))
}

// readExceptions decodes the num_exceptions exception values and their indexes
// shared by the variable-byte PFor blocks, OR-ing the high bits into out.
// Returns the number of input bytes consumed.
func readExceptions(in []byte, bitWidth, numExceptions uint, out []uint32, base int) int {
	var exceptions [blockSize]uint32
	pos := 0
	if in[0] == 255 {
		pos++
		for i := uint(0); i < numExceptions; i++ {
			exceptions[i] = readLE32(in[pos:])
			pos += 4
		}
	} else {
		for i := uint(0); i < numExceptions; i++ {
			v, n := readVB(in[pos:])
			exceptions[i] = v
			pos += n
		}
	}
	for i := uint(0); i < numExceptions; i++ {
		idx := uint(in[pos])
		pos++
		out[base+int(idx)] |= exceptions[i] << bitWidth
	}
	return pos
}

// decodePForVB handles a PFor block with variable-byte exceptions (partial
// blocks).
//
// Layout: bit width (6 bits) | type<<6, number of exceptions (8 bits), base
// values (num values of bitWidth bits, rounded up to a byte), exception values
// (255-prefixed 32-bit, or varbyte), then num_exc exception index bytes.
func decodePForVB(in []byte, num uint, out []uint32, base int) int {
	bitWidth := uint(in[0] & 0x3f)
	numExceptions := uint(in[1])
	pos := 2

	bs := newBitReader(in[pos:], bitWidth)
	for i := uint(0); i < num; i++ {
		out[base+int(i)] = bs.read()
	}
	pos += int(bytesForPackedBits(num, bitWidth))

	pos += readExceptions(in[pos:], bitWidth, numExceptions, out, base)

	prev := out[base-1]
	for i := uint(0); i < num; i++ {
		prev = out[base+int(i)] + prev + 1
		out[base+int(i)] = prev
	}
	return pos
}

// decodePForVBInterleaved handles a full PFor-VB block with the base values
// stored as four interleaved streams.
func decodePForVBInterleaved(in []byte, out []uint32, base int) int {
	bitWidth := uint(in[0] & 0x3f)
	numExceptions := uint(in[1])
	pos := 2

	body := in[pos:]
	bs0 := newInterleavedBitReader(body[0*4:], bitWidth)
	bs1 := newInterleavedBitReader(body[1*4:], bitWidth)
	bs2 := newInterleavedBitReader(body[2*4:], bitWidth)
	bs3 := newInterleavedBitReader(body[3*4:], bitWidth)
	for i := 0; i < blockSize/4; i++ {
		out[base+i*4+0] = bs0.read()
		out[base+i*4+1] = bs1.read()
		out[base+i*4+2] = bs2.read()
		out[base+i*4+3] = bs3.read()
	}
	pos += int(bytesForPackedBits(blockSize, bitWidth))

	pos += readExceptions(in[pos:], bitWidth, numExceptions, out, base)

	prev := out[base-1]
	for i := 0; i < blockSize; i++ {
		prev = out[base+i] + prev + 1
		out[base+i] = prev
	}
	return pos
}

// Decode decodes num strictly-increasing 32-bit document IDs from in, which
// must be plocate's interleaved delta-plus-1 PFor stream. The decoded values
// are returned in a freshly allocated slice of length num.
//
// in must have at least Slop readable bytes past the encoded posting list;
// plocate guarantees this by reading posting lists into buffers with 16 bytes
// of slop. Callers reading from a file should append at least Slop bytes of
// padding (see the plocate package).
func Decode(in []byte, num int) []uint32 {
	out := make([]uint32, num)
	if num == 0 {
		return out
	}
	// The first value is stored verbatim; the rest are delta-encoded relative
	// to it. We use a one-element prefix so that the per-block code can always
	// read out[base-1].
	buf := make([]uint32, num+1)
	base := 1

	val, n := readBaseval(in)
	buf[base] = val
	pos := n
	base++

	for i := 1; i < num; i += blockSize {
		numThisBlock := uint(blockSize)
		if rem := uint(num - i); rem < numThisBlock {
			numThisBlock = rem
		}
		full := numThisBlock == blockSize
		switch in[pos] >> 6 {
		case blockFOR:
			if full {
				pos += decodeFORInterleaved(in[pos:], buf, base)
			} else {
				pos += decodeFOR(in[pos:], numThisBlock, buf, base)
			}
		case blockPForVB:
			if full {
				pos += decodePForVBInterleaved(in[pos:], buf, base)
			} else {
				pos += decodePForVB(in[pos:], numThisBlock, buf, base)
			}
		case blockPForBitmap:
			if full {
				pos += decodePForBitmapInterleaved(in[pos:], buf, base)
			} else {
				pos += decodePForBitmap(in[pos:], numThisBlock, buf, base)
			}
		case blockConstant:
			pos += decodeConstant(in[pos:], numThisBlock, buf, base)
		}
		base += int(numThisBlock)
	}

	copy(out, buf[1:])
	return out
}
