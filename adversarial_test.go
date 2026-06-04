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
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// query opens path and runs a representative search, draining all results. It
// must never panic, regardless of how malformed the database is; any problem
// must surface as an error (from Open or Search) or as empty results.
func query(path string) (err error) {
	db, err := Open(path)
	if err != nil {
		return err
	}
	defer db.Close()
	// A mix of trigram, brute-force, glob and ignore-case queries to reach the
	// posting-list decoder, the hash table, and the zstd block reader.
	for _, pats := range [][]string{{"file"}, {"ab"}, {"a*b"}, {"data"}} {
		for _, ic := range []bool{false, true} {
			if e := db.Search(pats, Options{IgnoreCase: ic, Basename: ic}, func(string) bool { return true }); e != nil {
				err = e
			}
		}
	}
	return err
}

// TestAdversarialDatabases feeds the reader deliberately corrupted databases:
// every single-byte mutation in a sampling, truncations at many lengths, and
// fully random files. The reader is memory-safe Go (no cgo, no unsafe), so the
// only thing we assert here is that no input crashes the process — bad data
// must become an error, not a panic.
func TestAdversarialDatabases(t *testing.T) {
	c := getCorpora(t)["small"]
	good, err := os.ReadFile(c.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	runOnBytes := func(name string, b []byte) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		// A panic in query would propagate and fail the test; that is the
		// property under test. The returned error is expected and ignored.
		_ = query(p)
	}

	// 1. Single-byte mutations. Walk the whole header densely (it drives every
	// offset) and sample the body.
	for i := 0; i < len(good); i++ {
		if i >= headerSize && i%7 != 0 {
			continue
		}
		mutated := append([]byte(nil), good...)
		mutated[i] ^= 0xFF
		runOnBytes("mut", mutated)
	}

	// 2. Truncations at every length, including mid-header.
	for n := 0; n <= len(good); n += 1 + len(good)/512 {
		runOnBytes("trunc", good[:n])
	}

	// 3. Random files of various sizes, some carrying the real magic so that
	// they get past the magic check and exercise the offset handling.
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 500; i++ {
		n := rng.Intn(4096)
		b := make([]byte, n)
		rng.Read(b)
		if n >= headerSize && i%2 == 0 {
			copy(b, magic)
		}
		runOnBytes("rand", b)
	}
}
