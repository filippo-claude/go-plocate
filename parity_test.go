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
	"math/rand"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// runBinary runs prog with the given args and returns its stdout and exit code.
func runBinary(prog string, args ...string) (stdout []byte, code int) {
	cmd := exec.Command(prog, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out.Bytes(), ee.ExitCode()
		}
		// Unexpected failure; surface as a sentinel code.
		return out.Bytes(), -1
	}
	return out.Bytes(), 0
}

// assertParity runs the same query against the reference plocate and goplocate,
// and fails if stdout or exit code differ.
func assertParity(t *testing.T, dbPath string, args []string) {
	t.Helper()
	refArgs := append([]string{"-d", dbPath}, args...)
	refOut, refCode := runBinary(refPlocate, refArgs...)
	goOut, goCode := runBinary(goplocBin, refArgs...)
	if refCode != goCode || !bytes.Equal(refOut, goOut) {
		t.Errorf("parity mismatch for args %q\n  ref exit=%d go exit=%d\n--- ref stdout ---\n%s\n--- go stdout ---\n%s",
			args, refCode, goCode, capOutput(refOut), capOutput(goOut))
	}
}

func capOutput(b []byte) string {
	const max = 2000
	if len(b) > max {
		return string(b[:max]) + "\n...[truncated]"
	}
	return string(b)
}

// queryBattery is a fixed set of queries exercising the supported options.
var queryBattery = [][]string{
	{"file"},
	{"stdio.h"},
	{"README"},
	{".go"},
	{"-c", "file"},
	{"-i", "café"},
	{"-i", "CAFE"},
	{"-i", "makefile"},
	{"-b", "main"},
	{"-b", "-i", "casename"},
	{"-bc", "data"},
	{"-c", "-i", "TXT"},
	{"data", "config"},        // multiple patterns (AND)
	{"-i", "alpha", "bravo"},  // AND, ignore-case
	{"a*o"},                   // glob
	{"*.go"},                  // glob extension
	{"file_1?"},               // glob with ?
	{"-i", "*.TXT"},           // glob ignore-case
	{"[sd]td*.h"},             // glob char class
	{"zzzznomatch"},           // no matches
	{"-c", "zzzznomatch"},     // no matches, count
	{"ab"},                    // too short for trigrams (brute force)
	{"-i", "x"},               // single char, brute force
	{"-l", "3", "file"},       // limit
	{"-l", "1", "-c", "file"}, // limit + count
	{"-0", "file"},            // NUL separation
	{"deepfile"},              // deep path
	{"-b", "deepfile"},        // deep path basename
	{"über"},                  // unicode substring
	{"-i", "ÜBER"},            // unicode ignore-case
	{"東京"},                    // CJK
}

func TestParity(t *testing.T) {
	if !havePlocate() {
		t.Skipf("reference plocate not found at %s", refPlocate)
	}
	if goplocBin == "" {
		t.Skip("goplocate binary was not built")
	}
	cs := getCorpora(t)
	for _, name := range []string{"small", "big"} {
		c := cs[name]
		for _, args := range queryBattery {
			args := args
			t.Run(name+"/"+strings.Join(args, "_"), func(t *testing.T) {
				assertParity(t, c.dbPath, args)
			})
		}
	}
}

// TestFuzzParity draws random needles from substrings of real filenames in the
// corpus and compares output, exercising the trigram parser, decoder, and
// matcher against arbitrary inputs.
func TestFuzzParity(t *testing.T) {
	if !havePlocate() {
		t.Skipf("reference plocate not found at %s", refPlocate)
	}
	if goplocBin == "" {
		t.Skip("goplocate binary was not built")
	}
	c := getCorpora(t)["big"]

	// Collect all filenames from the database.
	db, err := Open(c.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var allNames []string
	for docid := 0; docid < db.NumFilenameBlocks(); docid++ {
		block, err := db.filenameBlock(uint32(docid))
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range bytes.Split(block, []byte{0}) {
			if len(n) > 0 {
				allNames = append(allNames, string(n))
			}
		}
	}
	db.Close()
	sort.Strings(allNames)
	if len(allNames) == 0 {
		t.Fatal("no filenames in corpus")
	}

	rng := rand.New(rand.NewSource(12345))
	const iterations = 400
	for i := 0; i < iterations; i++ {
		name := allNames[rng.Intn(len(allNames))]
		r := []rune(name)
		if len(r) < 2 {
			continue
		}
		start := rng.Intn(len(r) - 1)
		end := start + 1 + rng.Intn(len(r)-start)
		needle := string(r[start:end])

		var args []string
		if rng.Intn(2) == 0 {
			args = append(args, "-i")
		}
		if rng.Intn(2) == 0 {
			args = append(args, "-b")
		}
		args = append(args, needle)
		assertParity(t, c.dbPath, args)
	}
}
