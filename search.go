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
)

// Options controls how a search is performed.
type Options struct {
	// IgnoreCase makes matching and trigram generation case-insensitive.
	IgnoreCase bool
	// Basename matches against the final path component only (locate -b).
	Basename bool
}

// Search finds every filename in the database that matches all of the raw
// patterns, honoring opts, and calls visit for each match in database order.
// If visit returns false, the search stops early. The path passed to visit is
// valid only for the duration of the call.
//
// Matching mirrors the plocate binary: a pattern containing glob
// metacharacters is treated as an anchored glob, otherwise as a substring
// (wrapped as a case-folded glob when IgnoreCase is set).
func (db *DB) Search(patterns []string, opts Options, visit func(path string) bool) error {
	needles := make([]needle, len(patterns))
	for i, p := range patterns {
		needles[i] = buildNeedle(p, opts.IgnoreCase)
	}

	// Build the trigram disjunctions for all needles (AND-ed together).
	var groups []trigramDisjunction
	for _, n := range needles {
		groups = parseTrigrams(n.str, opts.IgnoreCase, groups)
	}
	dedupeGroups(&groups)

	wanted := usedTrigrams(groups)
	if len(wanted) == 0 {
		// Too short for trigram matching; brute-force every block.
		return db.scanAllBlocks(needles, opts, visit)
	}

	// Look up every wanted trigram once.
	type ptList struct {
		entry  trigram
		length uint64
	}
	found := map[uint32]ptList{}
	for trgm := range wanted {
		if e, length, ok := db.findTrigram(trgm); ok {
			found[trgm] = ptList{e, length}
		}
	}

	// Resolve each group to the present trigrams; if a group has none, nothing
	// can match.
	type resolvedGroup struct {
		present     []uint32
		maxNumDocid uint64
	}
	var resolved []resolvedGroup
	for _, g := range groups {
		var present []uint32
		var maxN uint64
		for _, t := range g.alternatives {
			if pl, ok := found[t]; ok {
				present = append(present, t)
				maxN += uint64(pl.entry.numDocids)
			}
		}
		if len(present) == 0 {
			return nil // a required trigram group is entirely absent
		}
		resolved = append(resolved, resolvedGroup{present, maxN})
	}

	// Process groups from smallest to largest, intersecting candidates.
	sort.SliceStable(resolved, func(i, j int) bool {
		return resolved[i].maxNumDocid < resolved[j].maxNumDocid
	})

	decodeCache := map[uint32][]uint32{}
	decode := func(t uint32) ([]uint32, error) {
		if d, ok := decodeCache[t]; ok {
			return d, nil
		}
		pl := found[t]
		d, err := db.decodePostingList(pl.entry, pl.length)
		if err != nil {
			return nil, err
		}
		decodeCache[t] = d
		return d, nil
	}

	var candidates []uint32
	for gi, g := range resolved {
		// Union of the present alternatives' posting lists.
		var union []uint32
		for _, t := range g.present {
			d, err := decode(t)
			if err != nil {
				return err
			}
			union = sortedUnion(union, d)
		}
		if gi == 0 {
			candidates = union
		} else {
			candidates = sortedIntersection(candidates, union)
		}
		if len(candidates) == 0 {
			return nil
		}
	}

	return db.scanBlocks(candidates, needles, opts, visit)
}

// dedupeGroups sorts and de-duplicates trigram groups by their alternatives,
// matching the unique_sort in plocate.
func dedupeGroups(groups *[]trigramDisjunction) {
	g := *groups
	sort.SliceStable(g, func(i, j int) bool {
		return lessUint32Slice(g[i].alternatives, g[j].alternatives)
	})
	out := g[:0]
	for i := range g {
		if i == 0 || !equalUint32Slice(g[i].alternatives, out[len(out)-1].alternatives) {
			out = append(out, g[i])
		}
	}
	*groups = out
}

// scanBlocks decompresses the given candidate blocks (in order) and reports the
// filenames that match every needle.
func (db *DB) scanBlocks(docids []uint32, needles []needle, opts Options, visit func(string) bool) error {
	for _, docid := range docids {
		block, err := db.filenameBlock(docid)
		if err != nil {
			return err
		}
		if !scanBlock(block, needles, opts, visit) {
			return nil
		}
	}
	return nil
}

// scanAllBlocks is the brute-force path used when the patterns are too short to
// generate any trigrams.
func (db *DB) scanAllBlocks(needles []needle, opts Options, visit func(string) bool) error {
	n := db.NumFilenameBlocks()
	for docid := uint32(0); int(docid) < n; docid++ {
		block, err := db.filenameBlock(docid)
		if err != nil {
			return err
		}
		if !scanBlock(block, needles, opts, visit) {
			return nil
		}
	}
	return nil
}

// scanBlock iterates the NUL-separated filenames in a decompressed block,
// invoking visit for each that matches all needles. It returns false if visit
// asked to stop.
func scanBlock(block []byte, needles []needle, opts Options, visit func(string) bool) bool {
	for len(block) > 0 {
		i := bytes.IndexByte(block, 0)
		var name []byte
		if i < 0 {
			name = block
			block = nil
		} else {
			name = block[:i]
			block = block[i+1:]
		}
		if len(name) == 0 && i < 0 {
			break
		}
		filename := string(name)
		haystack := filename
		if opts.Basename {
			if idx := bytes.LastIndexByte(name, '/'); idx >= 0 {
				haystack = string(name[idx+1:])
			}
		}
		matched := true
		for _, n := range needles {
			if !n.matches(haystack) {
				matched = false
				break
			}
		}
		if matched {
			if !visit(filename) {
				return false
			}
		}
	}
	return true
}

func lessUint32Slice(a, b []uint32) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

func equalUint32Slice(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sortedUnion returns the sorted union of two sorted, duplicate-free slices.
func sortedUnion(a, b []uint32) []uint32 {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make([]uint32, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}

// sortedIntersection returns the sorted intersection of two sorted slices.
func sortedIntersection(a, b []uint32) []uint32 {
	out := a[:0:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}
