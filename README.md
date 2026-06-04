# go-plocate

A **read-only, pure-Go reimplementation of the [plocate](https://plocate.sesse.net/)
database reader.** It opens a plocate database (as produced by plocate's
`updatedb` or `plocate-build`) and searches it, producing output identical to
the upstream `plocate` binary for the supported options.

The motivating use case is querying a plocate database on platforms where the
C++ implementation is inconvenient to build (e.g. macOS): `go-plocate` has no
cgo and no Linux-specific dependencies, and cross-compiles for Linux and macOS
(amd64/arm64).

> ⚠️ **This code was ported by an LLM agent (Anthropic's Claude), not written by
> hand by a human.** It is a machine translation of the upstream C++ reader into
> Go. It has been validated extensively against upstream plocate (see
> [Testing](#testing)), but it has **not** had line-by-line human review. Treat
> it accordingly: it is not robust against malicious or corrupt database files
> (neither is upstream), and you should not rely on it in security-sensitive
> contexts without your own review.

## Scope

Implemented (the read/query path only):

- Opening and parsing the database header (versions 0 and 1, `max_version` up to 2).
- Trigram hash-table lookup (the CRC-like `hash_trigram`).
- Posting-list decoding: the subset of TurboPFor that plocate uses
  (`decode_pfor_delta1_128`, interleaved, 32-bit, delta-plus-1).
- Filename blocks: zstd decompression, including databases that embed a trained
  zstd dictionary.
- Trigram generation for both case-sensitive and case-insensitive search.
- Matching: substring, anchored glob (`fnmatch`), and case-insensitive variants.

**Not** implemented:

- Building or updating databases (`updatedb`, `plocate-build`).
- Regular-expression patterns (`-r`/`--regexp`, `--regex`) — intentionally
  skipped.
- POSIX character classes / collating symbols inside globs (`[[:alpha:]]` etc.).

### Supported `goplocate` options

`-b/--basename`, `-i/--ignore-case`, `-c/--count`, `-d/--database` (colon-separated
list with backslash escaping, plus `LOCATE_PATH`), `-l/-n/--limit`,
`-0/--null`, `-N/--literal`, `-w/--wholename`, `-e/--existing`, `-A/--all`
(accepted and ignored), `--ignore-visibility`, `--help`, `--version`.

## Usage

```
go build ./cmd/goplocate
./goplocate -d /var/lib/plocate/plocate.db -i somefile
```

As a library:

```go
db, err := plocate.Open("/var/lib/plocate/plocate.db")
if err != nil { /* ... */ }
defer db.Close()

err = db.Search([]string{"needle"}, plocate.Options{IgnoreCase: true}, func(path string) bool {
    fmt.Println(path)
    return true // return false to stop early
})
```

Note that visibility checking (the `require_visibility` feature) and `--existing`
are applied by the `goplocate` command, not by the library `Search` method; the
library reports every filename that matches the patterns, and the caller decides
what to do with each.

## Testing

The tests are a **differential harness against upstream plocate**. They require
the upstream `plocate` and `updatedb` binaries; point the tests at them with:

```
GOPLOCATE_REF_PLOCATE=/path/to/plocate \
GOPLOCATE_REF_UPDATEDB=/path/to/updatedb \
go test ./...
```

(The defaults point at an in-tree meson build under `../plocate/obj`.) If the
binaries are absent, the dependent tests skip.

What the harness does:

- Builds synthetic corpora (tricky filenames: Unicode, spaces, glob
  metacharacters, deep nesting; and a large tree of thousands of files) and
  builds plocate databases from them with upstream `updatedb`. The large corpus
  is built twice so that the trained zstd dictionary is activated, and is large
  enough that posting lists span multiple full interleaved 128-value blocks —
  exercising the hard decoder paths. `TestDecoderCoverage` asserts this coverage
  rather than assuming it.
- **End-to-end parity** (`TestParity`): runs a battery of queries (substring,
  basename, ignore-case, multi-pattern AND, globs, brute-force short patterns,
  no-match, count, limit, NUL, Unicode/CJK) through both the upstream binary and
  `goplocate`, asserting byte-identical stdout and identical exit codes.
- **Decoder cross-check** (`TestPostingListCrossCheck`): independently derives,
  by brute force from the decompressed filename blocks, which blocks contain
  each trigram, then asserts the decoded posting list matches exactly. This
  validates the TurboPFor decoder, hash-table lookup, and zstd decode together
  without trusting any upstream internals.
- **Fuzz parity** (`TestFuzzParity`): random needles drawn from substrings of
  real filenames, compared against upstream.

## Provenance and references

- Upstream plocate by Steinar H. Gunderson: <https://git.sesse.net/plocate>
  (the upstream git server is IPv6-only).
- The posting-list format is the subset of TurboPFor described in Michael
  Stapelberg's analysis:
  <https://michael.stapelberg.ch/posts/2019-02-05-turbopfor-analysis/>.
  Stapelberg's own Go decoder,
  [`goturbopfor`](https://github.com/stapelberg/goturbopfor), targets a
  different variant (256-value blocks, no delta decoding) and is Apache-2.0
  licensed (incompatible with GPLv2); it was **not** copied. It was consulted
  only to cross-check understanding of the byte layout. This decoder is a port
  of plocate's own `turbopfor.cpp` scalar path.

## License

go-plocate is a **derivative work of plocate**, which is licensed under the
**GNU General Public License, version 2 or (at your option) any later version**.
Accordingly, go-plocate is distributed under the **same terms: GPL-2.0-or-later**.
See [`COPYING`](COPYING) for the full license text and [`NOTICE`](NOTICE) for
attribution and a summary of changes.

- Original plocate: Copyright 2020 Steinar H. Gunderson.
- Go port (this repository): Copyright 2026 Filippo Valsorda; produced by an LLM
  agent.
