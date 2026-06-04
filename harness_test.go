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

// This file sets up the differential test harness. The tests in this package
// validate the Go reader against the upstream plocate binaries: databases are
// built with upstream updatedb, and query output is compared byte-for-byte
// against upstream plocate.
//
// The reference binaries are located via environment variables, defaulting to
// the in-tree meson build:
//
//	GOPLOCATE_REF_PLOCATE   path to the upstream plocate binary
//	GOPLOCATE_REF_UPDATEDB  path to the upstream updatedb binary
//
// If a reference binary is missing, the dependent tests are skipped.

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

var (
	refPlocate  string
	refUpdatedb string
	goplocBin   string

	corpora     = map[string]*corpus{}
	corporaOnce sync.Once
	corporaErr  error
)

// corpus is a generated directory tree and the plocate database built from it.
type corpus struct {
	name     string
	root     string
	dbPath   string
	hasDict  bool
	numFiles int
}

func refBin(envvar, def string) string {
	if v := os.Getenv(envvar); v != "" {
		return v
	}
	return def
}

func TestMain(m *testing.M) {
	refPlocate = refBin("GOPLOCATE_REF_PLOCATE", "/home/exedev/plocate/obj/plocate")
	refUpdatedb = refBin("GOPLOCATE_REF_UPDATEDB", "/home/exedev/plocate/obj/updatedb")

	// Build the goplocate CLI once for the parity tests.
	tmp, err := os.MkdirTemp("", "goploc-bin")
	if err == nil {
		goplocBin = filepath.Join(tmp, "goplocate")
		cmd := exec.Command("go", "build", "-o", goplocBin, "./cmd/goplocate")
		if out, berr := cmd.CombinedOutput(); berr != nil {
			fmt.Fprintf(os.Stderr, "building goplocate failed: %v\n%s", berr, out)
			goplocBin = ""
		}
	}

	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

func haveUpdatedb() bool {
	_, err := os.Stat(refUpdatedb)
	return err == nil
}

func havePlocate() bool {
	_, err := os.Stat(refPlocate)
	return err == nil
}

// getCorpora builds the shared corpora and their databases once.
func getCorpora(t *testing.T) map[string]*corpus {
	t.Helper()
	if !haveUpdatedb() {
		t.Skipf("reference updatedb not found at %s", refUpdatedb)
	}
	corporaOnce.Do(func() {
		corpora, corporaErr = buildCorpora()
	})
	if corporaErr != nil {
		t.Fatalf("building corpora: %v", corporaErr)
	}
	return corpora
}

func buildCorpora() (map[string]*corpus, error) {
	out := map[string]*corpus{}
	base, err := os.MkdirTemp("", "goploc-corpora")
	if err != nil {
		return nil, err
	}

	// small: a handful of tricky filenames; no zstd dictionary.
	small, err := genTree(filepath.Join(base, "small"), smallNames())
	if err != nil {
		return nil, err
	}
	if err := buildDB(small); err != nil {
		return nil, err
	}
	out["small"] = small

	// big: thousands of files across a deep tree, enough to exceed 128 filename
	// blocks (so posting lists exercise full interleaved 128-value blocks) and
	// to make updatedb train a zstd dictionary. updatedb stores the trained
	// dictionary as "next_zstd_dictionary" and only activates it on the
	// following run, so we build twice to exercise the dictionary decode path.
	big, err := genTree(filepath.Join(base, "big"), bigNames(7000))
	if err != nil {
		return nil, err
	}
	if err := buildDB(big); err != nil {
		return nil, err
	}
	if err := buildDB(big); err != nil {
		return nil, err
	}
	out["big"] = big

	return out, nil
}

// genTree creates a directory tree containing the given relative paths and
// returns a corpus rooted there.
func genTree(root string, names []string) (*corpus, error) {
	for _, name := range names {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			return nil, err
		}
	}
	return &corpus{name: filepath.Base(root), root: root, numFiles: len(names)}, nil
}

// buildDB runs upstream updatedb over c.root and records dictionary presence.
func buildDB(c *corpus) error {
	c.dbPath = c.root + ".db"
	cmd := exec.Command(refUpdatedb,
		"-U", c.root,
		"--require-visibility", "no",
		"--config-file", "/dev/null",
		"-o", c.dbPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("updatedb %s: %v\n%s", c.root, err, out)
	}
	db, err := Open(c.dbPath)
	if err != nil {
		return fmt.Errorf("opening freshly built %s: %w", c.dbPath, err)
	}
	c.hasDict = db.hdr.zstdDictLengthBytes > 0
	db.Close()
	return nil
}

func smallNames() []string {
	names := []string{
		"a/b/c/stdio.h",
		"a/b/c/stdlib.h",
		"a/Makefile",
		"a/main.go",
		"d/README.md",
		"with space/foo bar.txt",
		"uni-café/naïve.txt",
		"uni-café/CAFÉ.DAT",
		"weird/[brackets].txt",
		"weird/star*name.txt",
		"weird/question?.md",
		"weird/back\\slash.bin",
		"deep/x/y/z/w/v/u/deepfile.conf",
		"Mixed/CaseName.TXT",
		"Mixed/casename.txt",
	}
	for i := 0; i < 200; i++ {
		names = append(names, fmt.Sprintf("d/file_%d.dat", i))
	}
	return names
}

func bigNames(n int) []string {
	rng := rand.New(rand.NewSource(0x70106a7e))
	words := []string{
		"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf",
		"hotel", "india", "juliet", "kilo", "lima", "mike", "november",
		"oscar", "papa", "quebec", "romeo", "sierra", "tango", "data",
		"config", "index", "cache", "build", "main", "test", "util",
		"café", "naïve", "über", "Москва", "東京",
	}
	exts := []string{".go", ".c", ".h", ".txt", ".md", ".dat", ".conf", ".bin", ""}
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		depth := 1 + rng.Intn(4)
		parts := make([]string, 0, depth+1)
		for d := 0; d < depth; d++ {
			parts = append(parts, words[rng.Intn(len(words))]+strconv.Itoa(rng.Intn(40)))
		}
		fname := words[rng.Intn(len(words))] + "_" + strconv.Itoa(i) + exts[rng.Intn(len(exts))]
		parts = append(parts, fname)
		names = append(names, filepath.Join(parts...))
	}
	return names
}
