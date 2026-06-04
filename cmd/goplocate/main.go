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
// Regular-expression matching (-r/--regexp and --regex) is intentionally not
// supported. See the repository README for details and licensing.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	plocate "github.com/filippo-claude/go-plocate"
	"golang.org/x/term"
)

const defaultDBFile = "/var/lib/plocate/plocate.db"

const version = "goplocate (go-plocate reader) 0.1.0"

type config struct {
	basename         bool
	count            bool
	ignoreCase       bool
	limit            int64
	null             bool
	literal          bool
	existing         bool
	ignoreVisibility bool
	dbpaths          []string
	patterns         []string
}

func usage(w *os.File) {
	fmt.Fprint(w, `Usage: goplocate [OPTION]... PATTERN...

  -b, --basename         search only the file name portion of path names
  -c, --count            print number of matches instead of the matches
  -d, --database DBPATH  search for files in DBPATH
                         (default is `+defaultDBFile+`)
  -e, --existing         only print entries for files that exist
  -i, --ignore-case      search case-insensitively
  -l, --limit LIMIT      stop after LIMIT matches
  -0, --null             delimit matches by NUL instead of newline
  -N, --literal          do not quote filenames, even if printing to a tty
  -w, --wholename        search the entire path name (default; see -b)
      --ignore-visibility  do not check directory visibility
      --help             print this help
      --version          print version information

Regular-expression patterns (-r/--regexp, --regex) are not supported.
`)
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cfg, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "goplocate: %v\n", err)
		return 1
	}
	if cfg == nil {
		return 0 // --help or --version already handled
	}

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
	vis := newVisibilityCache()

	for _, dbpath := range cfg.dbpaths {
		if cfg.limit > 0 && matched >= cfg.limit {
			break
		}
		db, err := plocate.Open(dbpath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "goplocate: %v\n", err)
			return 1
		}
		checkVis := db.CheckVisibility() && !cfg.ignoreVisibility

		serr := db.Search(cfg.patterns, opts, func(path string) bool {
			if checkVis && !vis.visible(path) {
				return true // not visible: skip, keep going
			}
			if cfg.existing {
				if _, err := os.Lstat(path); err != nil {
					return true
				}
			}
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

func parseArgs(args []string) (*config, error) {
	cfg := &config{}
	i := 0
	takeArg := func(inline string, hasInline bool) (string, error) {
		if hasInline {
			return inline, nil
		}
		if i+1 >= len(args) {
			return "", fmt.Errorf("option requires an argument")
		}
		i++
		return args[i], nil
	}
	parsingOpts := true
	for ; i < len(args); i++ {
		a := args[i]
		if !parsingOpts || a == "" || a[0] != '-' || a == "-" {
			cfg.patterns = append(cfg.patterns, a)
			continue
		}
		if a == "--" {
			parsingOpts = false
			continue
		}
		if strings.HasPrefix(a, "--") {
			name := a[2:]
			val := ""
			hasVal := false
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				val, hasVal = name[eq+1:], true
				name = name[:eq]
			}
			switch name {
			case "basename":
				cfg.basename = true
			case "count":
				cfg.count = true
			case "ignore-case":
				cfg.ignoreCase = true
			case "literal":
				cfg.literal = true
			case "null":
				cfg.null = true
			case "wholename":
				cfg.basename = false
			case "existing":
				cfg.existing = true
			case "all":
				// Accepted and ignored, as in plocate.
			case "ignore-visibility":
				cfg.ignoreVisibility = true
			case "database":
				v, err := takeArg(val, hasVal)
				if err != nil {
					return nil, err
				}
				cfg.dbpaths = parseDBPaths(v, cfg.dbpaths)
			case "limit":
				v, err := takeArg(val, hasVal)
				if err != nil {
					return nil, err
				}
				if err := setLimit(cfg, v); err != nil {
					return nil, err
				}
			case "regexp", "regex":
				return nil, fmt.Errorf("regular-expression matching is not supported")
			case "help":
				usage(os.Stdout)
				return nil, nil
			case "version":
				fmt.Println(version)
				return nil, nil
			default:
				return nil, fmt.Errorf("unrecognized option '--%s'", name)
			}
			continue
		}
		// Short option cluster.
		flags := a[1:]
		for j := 0; j < len(flags); j++ {
			c := flags[j]
			rest := flags[j+1:]
			switch c {
			case 'b':
				cfg.basename = true
			case 'c':
				cfg.count = true
			case 'i':
				cfg.ignoreCase = true
			case 'N':
				cfg.literal = true
			case '0':
				cfg.null = true
			case 'w':
				cfg.basename = false
			case 'e':
				cfg.existing = true
			case 'A':
				// Accepted and ignored.
			case 'd':
				v, err := takeArg(rest, rest != "")
				if err != nil {
					return nil, err
				}
				cfg.dbpaths = parseDBPaths(v, cfg.dbpaths)
				j = len(flags)
			case 'l', 'n':
				v, err := takeArg(rest, rest != "")
				if err != nil {
					return nil, err
				}
				if err := setLimit(cfg, v); err != nil {
					return nil, err
				}
				j = len(flags)
			case 'r':
				return nil, fmt.Errorf("regular-expression matching is not supported")
			default:
				return nil, fmt.Errorf("invalid option -- '%c'", c)
			}
		}
	}
	return cfg, nil
}

func setLimit(cfg *config, s string) error {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return fmt.Errorf("limit must be a strictly positive number")
	}
	cfg.limit = n
	return nil
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
