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
	"strings"
	"unicode"
)

// needleType selects how a needle is matched against a filename.
type needleType int

const (
	needleStrstr needleType = iota // plain substring
	needleGlob                     // anchored fnmatch glob
)

// needle is a compiled search pattern.
type needle struct {
	typ        needleType
	str        string
	ignoreCase bool // only meaningful for needleGlob (FNM_CASEFOLD)
}

// buildNeedle mirrors the per-pattern setup in plocate's main(): it decides
// whether a raw pattern is a substring or a glob, applies the ignore-case
// wrapping, and unescapes plain substrings. patternsAreRegex is unsupported by
// this reader and must be false.
func buildNeedle(raw string, ignoreCase bool) needle {
	if hasWildcard(raw) {
		return needle{typ: needleGlob, str: raw, ignoreCase: ignoreCase}
	}
	if ignoreCase {
		// strcasestr() mishandles locales, so plocate uses fnmatch("*needle*").
		return needle{typ: needleGlob, str: "*" + raw + "*", ignoreCase: true}
	}
	return needle{typ: needleStrstr, str: unescapeGlobToPlainString(raw)}
}

// hasWildcard reports whether raw contains an (unescaped) glob metacharacter,
// matching the detection in plocate's main().
func hasWildcard(raw string) bool {
	for i := 0; i < len(raw); {
		u, l := readUnigram(raw, i)
		if u == wildcardUnigram {
			return true
		}
		if l == 0 {
			break
		}
		i += l
	}
	return false
}

// unescapeGlobToPlainString removes glob escaping from a wildcard-free pattern,
// matching plocate's unescape_glob_to_plain_string.
func unescapeGlobToPlainString(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		u, l := readUnigram(s, i)
		// A wildcard-free pattern cannot contain wildcard unigrams, and a
		// premature end (trailing backslash) is a user error; plocate exits,
		// but for a library we simply stop.
		if u == wildcardUnigram || u == prematureEndUnigram {
			break
		}
		b.WriteByte(byte(u))
		i += l
	}
	return b.String()
}

// matches reports whether the needle matches haystack.
func (n needle) matches(haystack string) bool {
	switch n.typ {
	case needleStrstr:
		return strings.Contains(haystack, n.str)
	default: // needleGlob
		return fnmatch(n.str, haystack, n.ignoreCase)
	}
}

// fnmatch implements POSIX fnmatch(3) with flags 0 or FNM_CASEFOLD, as used by
// plocate (no FNM_PATHNAME, so '*' and '?' match '/' as well; no FNM_PERIOD).
// POSIX character classes ([[:alpha:]] etc.) and collating symbols are not
// supported.
func fnmatch(pattern, name string, caseFold bool) bool {
	return fnmatchRunes([]rune(pattern), []rune(name), caseFold)
}

func foldRune(r rune, caseFold bool) rune {
	if caseFold {
		return unicode.ToLower(r)
	}
	return r
}

func fnmatchRunes(p, s []rune, caseFold bool) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			// Collapse consecutive stars.
			for len(p) > 1 && p[1] == '*' {
				p = p[1:]
			}
			if len(p) == 1 {
				return true // trailing '*' matches the rest
			}
			// Try to match the remainder of the pattern at every position.
			for i := 0; i <= len(s); i++ {
				if fnmatchRunes(p[1:], s[i:], caseFold) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
			p, s = p[1:], s[1:]
		case '[':
			if len(s) == 0 {
				return false
			}
			matched, rest, ok := matchClass(p, s[0], caseFold)
			if !ok {
				// Not a valid class; treat '[' as a literal.
				if foldRune(p[0], caseFold) != foldRune(s[0], caseFold) {
					return false
				}
				p, s = p[1:], s[1:]
				continue
			}
			if !matched {
				return false
			}
			p, s = rest, s[1:]
		case '\\':
			if len(p) >= 2 {
				if len(s) == 0 || foldRune(p[1], caseFold) != foldRune(s[0], caseFold) {
					return false
				}
				p, s = p[2:], s[1:]
			} else {
				// Trailing backslash matches a literal backslash.
				if len(s) == 0 || s[0] != '\\' {
					return false
				}
				p, s = p[1:], s[1:]
			}
		default:
			if len(s) == 0 || foldRune(p[0], caseFold) != foldRune(s[0], caseFold) {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}

// matchClass evaluates a '[...]' bracket expression at the start of p against
// the character c. It returns whether c matched, the pattern remainder after
// the class, and ok=false if p does not start with a well-formed class.
func matchClass(p []rune, c rune, caseFold bool) (matched bool, rest []rune, ok bool) {
	// p[0] == '['
	i := 1
	negate := false
	if i < len(p) && (p[i] == '!' || p[i] == '^') {
		negate = true
		i++
	}
	// A ']' immediately after the (optional) negation is a literal.
	found := false
	first := true
	for {
		if i >= len(p) {
			return false, nil, false // unterminated class
		}
		if p[i] == ']' && !first {
			i++ // consume the closing bracket
			break
		}
		first = false

		var lo rune
		if p[i] == '\\' && i+1 < len(p) {
			lo = p[i+1]
			i += 2
		} else {
			lo = p[i]
			i++
		}

		// Range?
		if i+1 < len(p) && p[i] == '-' && p[i+1] != ']' {
			i++ // consume '-'
			var hi rune
			if p[i] == '\\' && i+1 < len(p) {
				hi = p[i+1]
				i += 2
			} else {
				hi = p[i]
				i++
			}
			if inRange(c, lo, hi, caseFold) {
				found = true
			}
		} else {
			if foldRune(lo, caseFold) == foldRune(c, caseFold) {
				found = true
			}
		}
	}
	return found != negate, p[i:], true
}

func inRange(c, lo, hi rune, caseFold bool) bool {
	if lo <= c && c <= hi {
		return true
	}
	if caseFold {
		cl := unicode.ToLower(c)
		cu := unicode.ToUpper(c)
		if (lo <= cl && cl <= hi) || (lo <= cu && cu <= hi) {
			return true
		}
	}
	return false
}
