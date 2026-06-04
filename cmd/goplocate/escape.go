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

package main

import (
	"bufio"
	"fmt"
	"unicode/utf8"
)

// printPossiblyEscaped writes name followed by a newline, quoting it for the
// shell when it contains unsafe characters. This is a port of plocate's
// print_possibly_escaped (serializer.cpp): safe strings are printed verbatim,
// otherwise they are wrapped in '...' using the $'...' construct for escapes,
// much like GNU ls.
func printPossiblyEscaped(out *bufio.Writer, name string) {
	if allSafe(name) {
		out.WriteString(name)
		out.WriteByte('\n')
		return
	}

	inEscaped := false
	out.WriteByte('\'')
	b := []byte(name)
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			out.WriteByte('?')
			b = b[1:]
			continue
		}
		c := b[0]
		if c < 32 || c == '\'' || c == '"' || c == '\\' {
			if !inEscaped {
				out.WriteString("'$'")
				inEscaped = true
			}
			switch c {
			case '\a':
				out.WriteString("\\a")
			case '\b':
				out.WriteString("\\b")
			case '\f':
				out.WriteString("\\f")
			case '\n':
				out.WriteString("\\n")
			case '\r':
				out.WriteString("\\r")
			case '\t':
				out.WriteString("\\t")
			case '\v':
				out.WriteString("\\v")
			case '\\':
				out.WriteString("\\\\")
			case '\'':
				out.WriteString("\\'")
			case '"':
				out.WriteString("\\\"")
			default:
				fmt.Fprintf(out, "\\%03o", c)
			}
		} else {
			if inEscaped {
				out.WriteString("''")
				inEscaped = false
			}
			out.Write(b[:size])
		}
		b = b[size:]
	}
	out.WriteString("'\n")
}

// allSafe reports whether every character in name is safe to print verbatim,
// i.e. not a control character, ', ", \ or `.
func allSafe(name string) bool {
	b := []byte(name)
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			return false // malformed data
		}
		if r < 32 || r == '\'' || r == '"' || r == '\\' || r == '`' {
			return false
		}
		b = b[size:]
	}
	return true
}
