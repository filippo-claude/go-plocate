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

package plocate

import "testing"

// TestDecoderCoverage asserts that the "big" corpus actually exercises the
// hard decoder paths: an active zstd dictionary and posting lists long enough
// to require full interleaved 128-value blocks. It also reports the longest
// posting list, as a tripwire against silently testing only trivial inputs.
func TestDecoderCoverage(t *testing.T) {
	c := getCorpora(t)["big"]
	db, err := Open(c.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if !c.hasDict {
		t.Errorf("big corpus has no active zstd dictionary; dictionary decode path is untested")
	}
	if db.NumFilenameBlocks() <= 128 {
		t.Errorf("big corpus has only %d blocks; want > 128 to exercise interleaved blocks", db.NumFilenameBlocks())
	}

	// Find the longest posting list by brute-force over the corpus trigrams.
	longest := 0
	var longestTrgm uint32
	counts := map[uint32]int{}
	n := db.NumFilenameBlocks()
	for docid := uint32(0); int(docid) < n; docid++ {
		block, err := db.filenameBlock(docid)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[uint32]bool{}
		for i := 0; i+3 <= len(block); i++ {
			if block[i] == 0 || block[i+1] == 0 || block[i+2] == 0 {
				continue
			}
			trgm := uint32(block[i]) | uint32(block[i+1])<<8 | uint32(block[i+2])<<16
			if !seen[trgm] {
				seen[trgm] = true
				counts[trgm]++
			}
		}
	}
	for trgm, c := range counts {
		if c > longest {
			longest, longestTrgm = c, trgm
		}
	}
	if longest <= 128 {
		t.Errorf("longest posting list is %d (<=128); interleaved full-block decode not exercised", longest)
	}

	// Decode the longest list to make sure that specific path runs cleanly.
	entry, length, ok := db.findTrigram(longestTrgm)
	if !ok {
		t.Fatalf("longest trigram %#06x not found", longestTrgm)
	}
	got := db.decodePostingList(entry, length)
	if len(got) != int(entry.numDocids) {
		t.Errorf("decoded %d docids, header says %d", len(got), entry.numDocids)
	}
	t.Logf("big corpus: %d blocks, active dict=%v, longest posting list=%d entries (trigram %#06x)",
		n, c.hasDict, longest, longestTrgm)
}
