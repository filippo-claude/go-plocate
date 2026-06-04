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

import (
	"bytes"
	"sort"
	"testing"
)

// TestPostingListCrossCheck validates the whole read path (hash table lookup,
// TurboPFor posting-list decoding, and zstd filename-block decompression)
// without trusting any upstream internals: it independently derives, by brute
// force, which filename blocks contain each trigram, then checks that the
// decoded posting list for that trigram matches exactly.
func TestPostingListCrossCheck(t *testing.T) {
	for _, name := range []string{"small", "big"} {
		t.Run(name, func(t *testing.T) {
			c := getCorpora(t)[name]
			db, err := Open(c.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			// Brute force: trigram -> set of block ids that contain it.
			want := map[uint32]map[uint32]bool{}
			n := db.NumFilenameBlocks()
			for docid := uint32(0); int(docid) < n; docid++ {
				block, err := db.filenameBlock(docid)
				if err != nil {
					t.Fatalf("block %d: %v", docid, err)
				}
				for _, name := range bytes.Split(block, []byte{0}) {
					for i := 0; i+3 <= len(name); i++ {
						trgm := uint32(name[i]) | uint32(name[i+1])<<8 | uint32(name[i+2])<<16
						set := want[trgm]
						if set == nil {
							set = map[uint32]bool{}
							want[trgm] = set
						}
						set[docid] = true
					}
				}
			}

			if len(want) == 0 {
				t.Fatal("no trigrams extracted from corpus")
			}

			checked := 0
			for trgm, set := range want {
				entry, length, ok := db.findTrigram(trgm)
				if !ok {
					t.Fatalf("trigram %#06x present in corpus but not found in hash table", trgm)
				}
				got, derr := db.decodePostingList(entry, length)
				if derr != nil {
					t.Fatalf("trigram %#06x: decode failed: %v", trgm, derr)
				}

				// got must be strictly increasing.
				for i := 1; i < len(got); i++ {
					if got[i] <= got[i-1] {
						t.Fatalf("trigram %#06x: decoded posting list not strictly increasing at %d: %v", trgm, i, got[i-1:i+1])
					}
				}

				wantList := make([]uint32, 0, len(set))
				for b := range set {
					wantList = append(wantList, b)
				}
				sort.Slice(wantList, func(i, j int) bool { return wantList[i] < wantList[j] })

				if !equalUint32Slice(got, wantList) {
					t.Fatalf("trigram %#06x: posting list mismatch\n got (%d): %v\nwant (%d): %v",
						trgm, len(got), trunc(got), len(wantList), trunc(wantList))
				}
				checked++
			}
			t.Logf("corpus %s: cross-checked %d distinct trigrams over %d blocks (dict=%v)",
				name, checked, n, c.hasDict)
		})
	}
}

func trunc(v []uint32) []uint32 {
	if len(v) > 20 {
		return v[:20]
	}
	return v
}
