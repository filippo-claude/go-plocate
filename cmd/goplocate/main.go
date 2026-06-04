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

// Command goplocate is a read-only, pure-Go reimplementation of the plocate
// query tool. It opens a plocate database (built by plocate's updatedb or
// plocate-build) and searches it, supporting the common locate(1) options.
//
// It is meant for querying foreign databases, so options that depend on the
// local filesystem (locate's --existing and its directory-visibility checks)
// are not implemented. Regular-expression matching (-r/--regexp, --regex) is
// also not supported.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	plocate "github.com/filippo-claude/go-plocate"
	"golang.org/x/term"
)

const defaultDBFile = "/var/lib/plocate/plocate.db"

const version = "goplocate (go-plocate reader) 0.1.0"

// dbPathsValue accumulates database paths. It may be given more than once, and
// each value is itself a colon-separated list (with backslash escaping), so
// "-d a:b -d c" searches a, b and c in order, matching plocate.
type dbPathsValue []string

func (d *dbPathsValue) String() string { return strings.Join(*d, ":") }

func (d *dbPathsValue) Set(s string) error {
	*d = parseDBPaths(s, *d)
	return nil
}

// limitValue parses a strictly-positive match limit shared by -l, -n and
// --limit.
type limitValue struct{ n *int64 }

func (l limitValue) String() string {
	if l.n == nil || *l.n == 0 {
		return "0"
	}
	return strconv.FormatInt(*l.n, 10)
}

func (l limitValue) Set(s string) error {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return fmt.Errorf("limit must be a strictly positive number")
	}
	*l.n = v
	return nil
}

type config struct {
	basename   bool
	count      bool
	ignoreCase bool
	limit      int64
	null       bool
	literal    bool
	dbpaths    dbPathsValue
	patterns   []string
}

const usageText = `Usage: goplocate [OPTION]... PATTERN...

  -b, --basename         search only the file name portion of path names
  -c, --count            print number of matches instead of the matches
  -d, --database DBPATH  search for files in DBPATH (may be repeated;
                         colon-separated; default ` + defaultDBFile + `)
  -i, --ignore-case      search case-insensitively
  -l, --limit LIMIT      stop after LIMIT matches
  -0, --null             delimit matches by NUL instead of newline
  -N, --literal          do not quote filenames, even if printing to a tty
  -w, --wholename        search the entire path name (default; see -b)
      --help             print this help
      --version          print version information

Regular-expression patterns (-r/--regexp, --regex) and the filesystem-dependent
options (-e/--existing, directory-visibility checks) are not supported.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func newFlagSet(cfg *config) (*flag.FlagSet, *bool) {
	fs := flag.NewFlagSet("goplocate", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usageText) }

	// -b/-w toggle the same field; using BoolFunc preserves left-to-right
	// precedence (e.g. "-b -w" ends up searching whole names).
	setBasename := func(v bool) func(string) error {
		return func(string) error { cfg.basename = v; return nil }
	}
	fs.BoolFunc("b", "", setBasename(true))
	fs.BoolFunc("basename", "", setBasename(true))
	fs.BoolFunc("w", "", setBasename(false))
	fs.BoolFunc("wholename", "", setBasename(false))

	for _, p := range []struct {
		short, long string
		dst         *bool
	}{
		{"c", "count", &cfg.count},
		{"i", "ignore-case", &cfg.ignoreCase},
		{"0", "null", &cfg.null},
		{"N", "literal", &cfg.literal},
	} {
		fs.BoolVar(p.dst, p.short, false, "")
		fs.BoolVar(p.dst, p.long, false, "")
	}

	fs.Var(&cfg.dbpaths, "d", "")
	fs.Var(&cfg.dbpaths, "database", "")

	lim := limitValue{n: &cfg.limit}
	fs.Var(lim, "l", "")
	fs.Var(lim, "n", "")
	fs.Var(lim, "limit", "")

	// Accepted and ignored, as in plocate.
	fs.BoolFunc("A", "", func(string) error { return nil })
	fs.BoolFunc("all", "", func(string) error { return nil })

	unsupported := func(string) error {
		return fmt.Errorf("regular-expression matching is not supported")
	}
	fs.BoolFunc("r", "", unsupported)
	fs.BoolFunc("regexp", "", unsupported)
	fs.BoolFunc("regex", "", unsupported)

	showVersion := fs.Bool("version", false, "")
	fs.BoolVar(showVersion, "V", false, "")

	return fs, showVersion
}

func run(args []string) int {
	cfg := &config{}
	fs, showVersion := newFlagSet(cfg)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			fmt.Print(usageText)
			return 0
		}
		fmt.Fprintf(os.Stderr, "goplocate: %v\n", err)
		return 1
	}
	if *showVersion {
		fmt.Println(version)
		return 0
	}

	cfg.patterns = fs.Args()
	if len(cfg.patterns) == 0 {
		fmt.Fprintln(os.Stderr, "goplocate: no pattern to search for specified")
		return 1
	}

	if len(cfg.dbpaths) == 0 {
		cfg.dbpaths = append(cfg.dbpaths, defaultDBFile)
	}
	if lp := os.Getenv("LOCATE_PATH"); lp != "" {
		cfg.dbpaths = parseDBPaths(lp, cfg.dbpaths)
	}

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	toTTY := !cfg.null && isatty(os.Stdout)
	opts := plocate.Options{IgnoreCase: cfg.ignoreCase, Basename: cfg.basename}

	var matched int64
	for _, dbpath := range cfg.dbpaths {
		if cfg.limit > 0 && matched >= cfg.limit {
			break
		}
		db, err := plocate.Open(dbpath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "goplocate: %v\n", err)
			return 1
		}
		serr := db.Search(cfg.patterns, opts, func(path string) bool {
			matched++
			if !cfg.count {
				printName(out, path, cfg, toTTY)
			}
			return cfg.limit <= 0 || matched < cfg.limit
		})
		db.Close()
		if serr != nil {
			out.Flush()
			fmt.Fprintf(os.Stderr, "goplocate: %v\n", serr)
			return 1
		}
	}

	if cfg.count {
		fmt.Fprintf(out, "%d\n", matched)
	}
	out.Flush()
	if matched == 0 {
		return 1
	}
	return 0
}

// parseDBPaths parses a colon-separated list of database paths, where a
// backslash escapes the next character, appending to out.
func parseDBPaths(s string, out []string) []string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case ':':
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteByte(s[i])
		}
	}
	out = append(out, b.String())
	return out
}

func printName(out *bufio.Writer, name string, cfg *config, toTTY bool) {
	if cfg.null {
		out.WriteString(name)
		out.WriteByte(0)
		return
	}
	if cfg.literal || !toTTY {
		out.WriteString(name)
		out.WriteByte('\n')
		return
	}
	printPossiblyEscaped(out, name)
}

func isatty(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}
