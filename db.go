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

// Package plocate is a read-only reimplementation of the plocate database
// reader: it opens a plocate database (as produced by plocate's updatedb or
// plocate-build) and searches it the same way the plocate binary does.
//
// It is a derivative work of plocate by Steinar H. Gunderson, ported to Go by
// an LLM agent (Claude). Only the query/read path is implemented; building or
// updating databases is out of scope. See the README and COPYING for details
// and licensing (GPL-2.0-or-later).
package plocate

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"

	"github.com/filippo-claude/go-plocate/internal/turbopfor"
	"github.com/klauspost/compress/zstd"
)

// headerSize is the size of the on-disk header struct, including trailing
// padding (the C++ struct aligns to 8 bytes; the last field is at offset 104).
const headerSize = 112

const magic = "\x00plocate"

// trigramEntrySize is sizeof(struct Trigram): trgm(4) + numDocids(4) + offset(8).
const trigramEntrySize = 16

// header mirrors the on-disk struct Header from plocate's db.h. Only the fields
// the reader needs are retained.
type header struct {
	version                  uint32
	hashtableSize            uint32
	extraHTSlots             uint32
	numDocids                uint32
	hashTableOffsetBytes     uint64
	filenameIndexOffsetBytes uint64
	maxVersion               uint32
	zstdDictLengthBytes      uint32
	zstdDictOffsetBytes      uint64
	checkVisibility          bool
}

func parseHeader(b []byte) (header, error) {
	if len(b) < headerSize {
		return header{}, fmt.Errorf("plocate: file too small to be a database")
	}
	if string(b[:8]) != magic {
		return header{}, fmt.Errorf("plocate: not a plocate database (bad magic)")
	}
	le := binary.LittleEndian
	h := header{
		version:                  le.Uint32(b[8:]),
		hashtableSize:            le.Uint32(b[12:]),
		extraHTSlots:             le.Uint32(b[16:]),
		numDocids:                le.Uint32(b[20:]),
		hashTableOffsetBytes:     le.Uint64(b[24:]),
		filenameIndexOffsetBytes: le.Uint64(b[32:]),
		maxVersion:               le.Uint32(b[40:]),
		zstdDictLengthBytes:      le.Uint32(b[44:]),
		zstdDictOffsetBytes:      le.Uint64(b[48:]),
		checkVisibility:          b[104] != 0,
	}
	// plocate only understands database versions 0 and 1.
	if h.version != 0 && h.version != 1 {
		return header{}, fmt.Errorf("plocate: database has version %d, expected 0 or 1; please rebuild it", h.version)
	}
	if h.version == 0 {
		// These fields are junk in version 0.
		h.zstdDictOffsetBytes = 0
		h.zstdDictLengthBytes = 0
	}
	if h.maxVersion < 2 {
		// check_visibility (and the other max_version 2 fields) are junk.
		h.checkVisibility = true
	}
	return h, nil
}

// DB is an opened, read-only plocate database. It is safe for concurrent use.
type DB struct {
	f    *os.File
	data []byte // mmap of the whole file
	hdr  header
	dec  *zstd.Decoder
}

// Open opens the plocate database at path for reading.
func Open(path string) (*DB, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := fi.Size()
	if size < headerSize {
		f.Close()
		return nil, fmt.Errorf("plocate: %s is too small to be a database", path)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("plocate: mmap %s: %w", path, err)
	}
	hdr, err := parseHeader(data)
	if err != nil {
		syscall.Munmap(data)
		f.Close()
		return nil, err
	}

	var dopts []zstd.DOption
	dopts = append(dopts, zstd.WithDecoderConcurrency(0))
	if hdr.zstdDictLengthBytes > 0 {
		end := hdr.zstdDictOffsetBytes + uint64(hdr.zstdDictLengthBytes)
		if end > uint64(len(data)) {
			syscall.Munmap(data)
			f.Close()
			return nil, fmt.Errorf("plocate: zstd dictionary extends past end of file")
		}
		dict := make([]byte, hdr.zstdDictLengthBytes)
		copy(dict, data[hdr.zstdDictOffsetBytes:end])
		dopts = append(dopts, zstd.WithDecoderDicts(dict))
	}
	dec, err := zstd.NewReader(nil, dopts...)
	if err != nil {
		syscall.Munmap(data)
		f.Close()
		return nil, err
	}

	return &DB{f: f, data: data, hdr: hdr, dec: dec}, nil
}

// Close releases the resources associated with the database.
func (db *DB) Close() error {
	db.dec.Close()
	err := syscall.Munmap(db.data)
	if cerr := db.f.Close(); err == nil {
		err = cerr
	}
	db.data = nil
	return err
}

// CheckVisibility reports whether the database requests visibility checking
// (i.e. plocate would stat the parent directories before reporting a file).
func (db *DB) CheckVisibility() bool { return db.hdr.checkVisibility }

// NumFilenameBlocks returns the number of filename blocks (document IDs).
func (db *DB) NumFilenameBlocks() int { return int(db.hdr.numDocids) }

// hashTrigram hashes a trigram into a hash-table bucket, matching db.h's
// hash_trigram (a CRC-like computation).
func hashTrigram(trgm, htSize uint32) uint32 {
	crc := trgm
	for i := 0; i < 32; i++ {
		bit := crc&0x80000000 != 0
		crc <<= 1
		if bit {
			crc ^= 0x1edc6f41
		}
	}
	return crc % htSize
}

// trigram is one hash-table entry (struct Trigram).
type trigram struct {
	trgm      uint32
	numDocids uint32
	offset    uint64
}

func (db *DB) readTrigramEntry(index uint64) trigram {
	off := db.hdr.hashTableOffsetBytes + index*trigramEntrySize
	b := db.data[off:]
	le := binary.LittleEndian
	return trigram{
		trgm:      le.Uint32(b[0:]),
		numDocids: le.Uint32(b[4:]),
		offset:    le.Uint64(b[8:]),
	}
}

// findTrigram looks up trgm in the hash table. It returns the entry and the
// byte length of its posting list, or ok=false if the trigram is absent.
//
// This mirrors Corpus::find_trigram: starting at the hashed bucket, scan up to
// extraHTSlots+1 consecutive entries; the posting list length is the gap to the
// next entry's offset.
func (db *DB) findTrigram(trgm uint32) (entry trigram, length uint64, ok bool) {
	bucket := uint64(hashTrigram(trgm, db.hdr.hashtableSize))
	for i := uint64(0); i < uint64(db.hdr.extraHTSlots)+1; i++ {
		e := db.readTrigramEntry(bucket + i)
		if e.trgm == trgm {
			next := db.readTrigramEntry(bucket + i + 1)
			return e, next.offset - e.offset, true
		}
	}
	return trigram{}, 0, false
}

// decodePostingList reads and decodes the posting list for the given entry,
// returning the list of (strictly increasing) document IDs it contains.
func (db *DB) decodePostingList(entry trigram, length uint64) []uint32 {
	start := entry.offset
	end := start + length
	// The TurboPFor decoder may read up to turbopfor.Slop bytes past the end of
	// the posting list. Copy into a padded buffer so we never read out of the
	// mmap, even for the last list in the file.
	buf := make([]byte, length+2*turbopfor.Slop)
	copy(buf, db.data[start:end])
	return turbopfor.Decode(buf, int(entry.numDocids))
}

// filenameBlock returns the decompressed, NUL-separated filenames for the block
// with the given document ID.
func (db *DB) filenameBlock(docid uint32) ([]byte, error) {
	idxOff := db.hdr.filenameIndexOffsetBytes + uint64(docid)*8
	le := binary.LittleEndian
	off := le.Uint64(db.data[idxOff:])
	nextOff := le.Uint64(db.data[idxOff+8:])
	compressed := db.data[off:nextOff]
	out, err := db.dec.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("plocate: decompressing filename block %d: %w", docid, err)
	}
	return out, nil
}
