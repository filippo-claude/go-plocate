# The plocate database format

This document describes the on-disk format of a plocate database, at the level
of detail needed to write a reader. It is derived from plocate's `db.h`,
`turbopfor.cpp`, and `database-builder.cpp`, and was validated against real
databases built by upstream `updatedb`. It describes the format as understood by
plocate's reader (database `version` 0 and 1, with optional `max_version` 2
extensions).

All integers are little-endian. Byte offsets into the file are absolute unless
noted.

## 1. Overview

A plocate database maps **trigrams** (3-byte sequences) to the set of
**filename blocks** that contain them. Search proceeds by intersecting the
posting lists of the trigrams in the query, then decompressing the candidate
filename blocks and matching the query against the individual filenames. A
"document" in posting-list terminology is a *block* of filenames, identified by
a **document ID** (docid) that is simply the block index.

The file contains, in no particular required order after the header:

- a fixed-size **header**;
- a **hash table** mapping trigrams to posting lists;
- the **posting lists** (TurboPFor-compressed docid lists);
- a **filename index** (one offset per block, plus a sentinel);
- the **filename blocks** (zstd-compressed);
- optionally a trained **zstd dictionary**;
- `max_version >= 2` only: `directory_data`, a `conf_block`, and a
  `next_zstd_dictionary`. These are used only by `updatedb` for incremental
  rebuilds and are ignored by a reader.

## 2. Header

The header is a C struct written verbatim, 112 bytes including trailing padding.
Field offsets:

| Offset | Size | Field | Notes |
|-------:|-----:|-------|-------|
| 0  | 8 | `magic` | the bytes `\0plocate` |
| 8  | 4 | `version` | 0 or 1; a reader rejects anything else |
| 12 | 4 | `hashtable_size` | number of hash buckets |
| 16 | 4 | `extra_ht_slots` | extra probe slots after each bucket |
| 20 | 4 | `num_docids` | number of filename blocks |
| 24 | 8 | `hash_table_offset_bytes` | offset of the hash table |
| 32 | 8 | `filename_index_offset_bytes` | offset of the filename index |
| 40 | 4 | `max_version` | 1 or 2 (forward-compatible feature level) |
| 44 | 4 | `zstd_dictionary_length_bytes` | 0 if no dictionary |
| 48 | 8 | `zstd_dictionary_offset_bytes` | offset of the dictionary |
| 56 | 8 | `directory_data_length_bytes` | `max_version >= 2`; updatedb only |
| 64 | 8 | `directory_data_offset_bytes` | `max_version >= 2`; updatedb only |
| 72 | 8 | `next_zstd_dictionary_length_bytes` | `max_version >= 2`; updatedb only |
| 80 | 8 | `next_zstd_dictionary_offset_bytes` | `max_version >= 2`; updatedb only |
| 88 | 8 | `conf_block_length_bytes` | `max_version >= 2`; updatedb only |
| 96 | 8 | `conf_block_offset_bytes` | `max_version >= 2`; updatedb only |
| 104 | 1 | `check_visibility` | `max_version >= 2`; see §7 |

Reader rules:

- If `version == 0`, treat `zstd_dictionary_offset_bytes` and
  `zstd_dictionary_length_bytes` as 0 (the fields hold junk).
- If `max_version < 2`, treat `check_visibility` as `true` (the field holds
  junk).

## 3. Hash table

The hash table is an array of **Trigram entries**, each 16 bytes:

| Offset | Size | Field |
|-------:|-----:|-------|
| 0 | 4 | `trgm` (the trigram value) |
| 4 | 4 | `num_docids` (length of the posting list, in docids) |
| 8 | 8 | `offset` (absolute file offset of the posting list) |

A trigram value packs three bytes `b0,b1,b2` as `b0 | b1<<8 | b2<<16`.

### Hash function

```
hash_trigram(trgm, ht_size):
    crc = trgm
    repeat 32 times:
        bit = crc & 0x80000000
        crc <<= 1            # 32-bit
        if bit: crc ^= 0x1edc6f41
    return crc % ht_size
```

### Lookup

To find a trigram `T`, compute `bucket = hash_trigram(T, hashtable_size)`, then
scan entries `bucket, bucket+1, … bucket+extra_ht_slots` (i.e. up to
`extra_ht_slots+1` entries). If an entry's `trgm == T`, it is a hit. The posting
list occupies the bytes `[entry.offset, next_entry.offset)`, where `next_entry`
is the immediately following table entry — so the reader must be able to read
one entry past the matched slot. The table is allocated with enough padding that
reading `extra_ht_slots + 2` consecutive entries from any bucket is in bounds.

If no entry in the probe window matches, the trigram is absent (its posting list
is empty).

## 4. Posting lists (the TurboPFor subset)

Each posting list encodes `num_docids` **strictly increasing** 32-bit docids
using a single TurboPFor codec: 128-value blocks, "delta plus 1" decoding,
interleaved bit packing for full blocks. plocate's encoder only ever emits this
one codec; the reader only needs to decode it.

> The decoder may read up to **16 bytes past the end** of the posting list, so a
> reader must provide that much readable slop (plocate reads posting lists into
> buffers with 16 slop bytes; this port copies each list into a zero-padded
> buffer).

### 4.1 Layout

```
posting_list := baseval block*
```

- `baseval` is the first docid, encoded as in §4.2.
- Each `block` decodes the next up to 128 docids. The first byte of a block is
  `(type << 6) | bit_width`, where `bit_width` is 6 bits and `type` is one of:

  | type | name | meaning |
  |-----:|------|---------|
  | 0 | FOR | bit-packed, no exceptions |
  | 1 | PFOR_VB | bit-packed + variable-byte exceptions |
  | 2 | PFOR_BITMAP | bit-packed + bitmap exceptions |
  | 3 | CONSTANT | all deltas identical |

- A block that holds exactly 128 values (a *full* block, i.e. not the final
  partial block) uses the **interleaved** bit layout (§4.5) for FOR, PFOR_VB and
  PFOR_BITMAP. The final block, if it has fewer than 128 values, uses the plain
  (non-interleaved) layout. CONSTANT is never interleaved.

### 4.2 Delta-plus-1 decoding

After a block's *base values* `v[i]` are unpacked (and exceptions applied), the
final docids are produced by a prefix sum with an implicit `+1` per step, since
the docids strictly increase:

```
out[i] = v[i] + out[i-1] + 1
```

where `out[-1]` for the first block is `baseval`, and across blocks it is the
last docid of the previous block.

### 4.3 Base value encoding (`read_baseval`)

A PrefixVarint-like encoding of the first docid (values below 2^28):

| First byte range | Bytes | Value |
|------------------|------:|-------|
| `< 128`  | 1 | `b0` |
| `< 192`  | 2 | `(b0<<8 | b1) & 0x3fff` |
| `< 224`  | 3 | `(b0<<16 | b2<<8 | b1) & 0x1fffff` |
| `< 240`  | 4 | `(b0<<24 | b1<<16 | b2<<8 | b3) & 0x0fffffff` |

### 4.4 Variable-byte encoding (`read_vb`)

Used for exception values:

| First byte | Bytes | Value |
|------------|------:|-------|
| `<= 176` | 1 | `b0` |
| `<= 240` | 2 | `((b0-177)<<8 | b1) + 177` |
| `<= 248` | 3 | `((b0-241)<<16 | le16(b1..b2)) + 16561` |
| `== 249` | 4 | `b1 | b2<<8 | b3<<16` |
| `== 250` | 5 | `le32(b1..b4)` |

### 4.5 Bit readers

**Plain** (`BitReader`): reads consecutive `bit_width`-wide little-endian values
from a byte stream. Conceptually, maintain a bit cursor; each read takes the next
`bit_width` bits, least-significant first, from `le32(in[pos:])`, then advances
`pos` and the in-byte bit offset.

**Interleaved** (`InterleavedBitReader`, 4 streams): the packed values are
distributed round-robin over four parallel 32-bit-word streams (stride = 16
bytes). A reader for stream *s* starts at byte `4*s` and, on each read, advances
by `16 * (bits_used / 32)` bytes, taking `bit_width` bits across a 32-bit-word
boundary when needed. A full interleaved block decodes four positions at a time:

```
for i in 0 .. 31:
    out[i*4 + 0] = bs0.read(); out[i*4 + 1] = bs1.read()
    out[i*4 + 2] = bs2.read(); out[i*4 + 3] = bs3.read()
```

The four readers start at `in+0, in+4, in+8, in+12` respectively.

### 4.6 Block types

Let `bit_width` be the low 6 bits of the block's first byte.

**CONSTANT** — first byte, then one base value of `bit_width` bits (rounded up to
a byte). Every output delta equals that value:
`out[i] = val + out[i-1] + 1`.

**FOR** — first byte, then `num` (or 128) base values of `bit_width` bits each,
read with the plain (or interleaved, for full blocks) reader, then delta-decode.

**PFOR_VB** — first byte, then a 1-byte `num_exceptions`, then the base values
(`bit_width` bits each, plain or interleaved). Then the exceptions:
- if the next byte is `255`: consume it, then `num_exceptions` raw little-endian
  32-bit values;
- otherwise: `num_exceptions` `read_vb`-encoded values.
Then `num_exceptions` single index bytes; for each, OR the exception into the
base value at that index: `out[idx] |= exc << bit_width`. Finally delta-decode.

**PFOR_BITMAP** — first byte, then a 1-byte `exception_bit_width`, then a bitmap
of `num` bits (rounded up to a byte) marking which positions are exceptional,
then the exception values (packed at `exception_bit_width`, one per set bit, in
position order), then the base values (`bit_width` bits, plain or interleaved).
Each exceptional position's high bits come from the bitmap section and its low
bits from the base values: effectively
`out[i] = ((exception_high[i]) << bit_width) | base[i]`, then delta-decode.
(The decoder writes the exception high bits into `out` first, then ORs in the
base values and prefix-sums.)

## 5. Filename index and blocks

At `filename_index_offset_bytes` there is an array of `num_docids + 1`
little-endian `uint64` file offsets. Block `d` (its docid) occupies the bytes
`[offset[d], offset[d+1])`; the final entry is a sentinel so that every block,
including the last, has an end offset.

Each block is an independent **zstd frame**. Decompressing it yields the block's
filenames as **NUL-separated** byte strings (the frame's content size is present
in the frame header). A reader splits on `\0`. Filenames are full path strings.

### zstd dictionary

If `zstd_dictionary_length_bytes > 0`, the bytes at `zstd_dictionary_offset_bytes`
are a standard trained zstd dictionary; every filename block is compressed with
it and must be decompressed with it registered. (Small databases have no
dictionary. `updatedb` trains a dictionary and stores it as the
`next_zstd_dictionary` of the database it writes; it becomes the active
dictionary on the *following* `updatedb` run.)

## 6. Query algorithm

1. Turn each pattern into trigrams (see plocate's `parse_trigrams`): an AND of
   OR-groups. Case-insensitive search yields, per group, the trigrams of all
   case variants (OR-ed together). Trigrams are over bytes. If a pattern is too
   short to yield any trigram, fall back to scanning every block.
2. Look up each distinct trigram in the hash table. If any AND-group has *no*
   present trigram, the result is empty.
3. For each group, union the posting lists of its present trigrams; intersect
   the groups' unions to get candidate docids. (Processing groups smallest-first
   is an optimization.)
4. For each candidate docid in increasing order, decompress its filename block
   and test every filename against all patterns (substring, glob, or
   case-folded glob). Trigrams are necessary but not sufficient, so this final
   match removes false positives. Emit matches in block order, then file order
   within a block.

## 7. Visibility (`check_visibility`)

When `check_visibility` is true, plocate only reports a file if every ancestor
directory is readable and searchable by the calling user (`access(dir,
R_OK|X_OK)` for each path prefix ending in `/`, excluding the file itself). This
is a privacy feature of the live system database; it depends on filesystem
state, not on the database contents. A reader may replicate it, expose it, or
ignore it.

## 8. Out of scope for a reader

`directory_data`, `conf_block`, and `next_zstd_dictionary` (all `max_version 2`)
exist solely to let `updatedb` perform fast incremental rebuilds and to carry the
configuration forward; they are not needed to query the database.
