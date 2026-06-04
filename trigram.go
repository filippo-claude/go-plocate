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
	"sort"
	"unicode"
)

// This file is a port of plocate's parse_trigrams.cpp. It breaks a search
// needle into a set of trigrams (3-byte sequences) that must be present for a
// filename to match. Trigrams are grouped into disjunctions: within a group the
// trigrams are OR-ed (alternatives, used for case-insensitive search), and the
// groups are AND-ed together. The trigrams are necessary but not sufficient, so
// false positives are filtered later by matching the needle against filenames.
//
// Note that trigrams are over bytes, not Unicode code points, exactly as in the
// original.

const (
	wildcardUnigram     = 0xFF000000
	prematureEndUnigram = 0xFF000001
)

// trigramDisjunction is one OR-group of candidate trigrams.
type trigramDisjunction struct {
	alternatives []uint32
}

// readUnigram reads a single unigram from s starting at byte offset start,
// taking escaping into account (\x becomes x). It returns wildcardUnigram for a
// glob metacharacter (?, * or a [...] group) and prematureEndUnigram if it runs
// past the end of the string. The second return value is the byte length
// consumed.
func readUnigram(s string, start int) (uint32, int) {
	if start >= len(s) {
		return prematureEndUnigram, 0
	}
	switch s[start] {
	case '\\':
		// Escaped character.
		if start+1 >= len(s) {
			return prematureEndUnigram, 1
		}
		return uint32(s[start+1]), 2
	case '*', '?':
		return wildcardUnigram, 1
	case '[':
		// Character class; scan to find the end.
		length := 1
		if start+length >= len(s) {
			return prematureEndUnigram, length
		}
		if s[start+length] == '!' {
			length++
		}
		if start+length >= len(s) {
			return prematureEndUnigram, length
		}
		if s[start+length] == ']' {
			length++
		}
		for {
			if start+length >= len(s) {
				return prematureEndUnigram, length
			}
			if s[start+length] == ']' {
				return wildcardUnigram, length + 1
			}
			length++
		}
	}
	return uint32(s[start]), 1
}

// readTrigram reads a trigram (three unigrams) starting at start. It returns
// wildcardUnigram or prematureEndUnigram if either occurred while reading.
func readTrigram(s string, start int) uint32 {
	u1, l1 := readUnigram(s, start)
	if u1 == wildcardUnigram || u1 == prematureEndUnigram {
		return u1
	}
	u2, l2 := readUnigram(s, start+l1)
	if u2 == wildcardUnigram || u2 == prematureEndUnigram {
		return u2
	}
	u3, _ := readUnigram(s, start+l1+l2)
	if u3 == wildcardUnigram || u3 == prematureEndUnigram {
		return u3
	}
	return u1 | (u2 << 8) | (u3 << 16)
}

func uniqueSortUint32(v []uint32) []uint32 {
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	out := v[:0]
	for i, x := range v {
		if i == 0 || x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}

// parseTrigrams breaks needle into trigram disjunctions, appending them to
// groups.
func parseTrigrams(needle string, ignoreCase bool, groups []trigramDisjunction) []trigramDisjunction {
	if ignoreCase {
		return parseTrigramsIgnoreCase(needle, groups)
	}
	// The case-sensitive case is straightforward: every trigram in the needle.
	for i := 0; i < len(needle); {
		trgm := readTrigram(needle, i)
		_, l := readUnigram(needle, i)
		if l == 0 {
			break
		}
		i += l
		if trgm == wildcardUnigram || trgm == prematureEndUnigram {
			continue
		}
		groups = append(groups, trigramDisjunction{alternatives: []uint32{trgm}})
	}
	return groups
}

type trigramState struct {
	buffered      string
	nextCodepoint int
}

func lessTrigramState(a, b trigramState) bool {
	if a.nextCodepoint != b.nextCodepoint {
		return a.nextCodepoint < b.nextCodepoint
	}
	return a.buffered < b.buffered
}

// parseTrigramsIgnoreCase is the case-insensitive counterpart. It performs
// inverse case folding on each code point to find legal byte alternatives and
// then generates the candidate trigram sets. See the comments in
// parse_trigrams.cpp for the rationale.
func parseTrigramsIgnoreCase(needle string, groups []trigramDisjunction) []trigramDisjunction {
	var alternativesForCP [][]string
	for _, ch := range needle {
		var alt []string
		alt = append(alt, string(ch))
		if lower := unicode.ToLower(ch); lower != ch {
			alt = append(alt, string(lower))
		}
		if upper := unicode.ToUpper(ch); upper != ch && upper != unicode.ToLower(ch) {
			alt = append(alt, string(upper))
		}
		alternativesForCP = append(alternativesForCP, alt)
	}

	states := []trigramState{{buffered: "", nextCodepoint: 0}}

	for {
		// Extend every state so it has buffered at least three bytes.
		var needAnotherPass bool
		for {
			needAnotherPass = false
			var newStates []trigramState
			for _, state := range states {
				if readTrigram(state.buffered, 0) != prematureEndUnigram {
					newStates = append(newStates, state)
					continue
				}
				if state.nextCodepoint == len(alternativesForCP) {
					// Cannot form a complete trigram from this alternative; done.
					return groups
				}
				for _, rune_ := range alternativesForCP[state.nextCodepoint] {
					newState := trigramState{buffered: state.buffered + rune_, nextCodepoint: state.nextCodepoint + 1}
					if readTrigram(state.buffered, 0) == prematureEndUnigram {
						needAnotherPass = true
					}
					newStates = append(newStates, newState)
				}
			}
			states = newStates
			if !needAnotherPass {
				break
			}
		}

		// Every state now has at least three bytes, so we have a complete set
		// of trigrams; the filename must contain at least one. Emit them, strip
		// the first unigram, and deduplicate before continuing.
		anyWildcard := false
		var alternatives []uint32
		for i := range states {
			trgm := readTrigram(states[i].buffered, 0)
			alternatives = append(alternatives, trgm)
			_, l := readUnigram(states[i].buffered, 0)
			states[i].buffered = states[i].buffered[l:]
			if trgm == wildcardUnigram {
				anyWildcard = true
			}
		}
		alternatives = uniqueSortUint32(alternatives)
		states = uniqueSortStates(states)

		if !anyWildcard {
			groups = append(groups, trigramDisjunction{alternatives: alternatives})
		}

		if len(states) > 100 {
			// A pathological pattern; give up generating more trigrams.
			return groups
		}
	}
}

func uniqueSortStates(v []trigramState) []trigramState {
	sort.Slice(v, func(i, j int) bool { return lessTrigramState(v[i], v[j]) })
	out := v[:0]
	for i := range v {
		if i == 0 || v[i] != out[len(out)-1] {
			out = append(out, v[i])
		}
	}
	return out
}

// usedTrigrams returns the set of distinct trigram values referenced by groups.
func usedTrigrams(groups []trigramDisjunction) map[uint32]bool {
	set := map[uint32]bool{}
	for _, g := range groups {
		for _, t := range g.alternatives {
			set[t] = true
		}
	}
	return set
}
