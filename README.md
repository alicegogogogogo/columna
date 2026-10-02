# Columna

Columna is a self-describing columnar file format with a query engine on top:
typed rows are written into `.columna` files (magic, schema, row groups,
per-column data pages, footer statistics), read back losslessly, and queried
with projection, predicates and aggregates over JSON/HTTP or the command line.

The contract in one glance: a table is one `<name>.columna` file in the service
data directory; a write batch is a row group; pages hold at most
`max_page_values` values per column (1024 by default) and record min/max/NULL
statistics that the footer repeats, so a query can prune pages; encodings are
`plain`, `dictionary` and `rle`; predicates are `=`, `<`, `>`, `<=`, `>=` and
`between`; aggregates are `count`, `sum`, `min` and `max`; and the rows a query
returns are identical with pruning on or off.

## Requirements

- Go 1.22 or newer; standard library only (no third-party modules, no cgo)
- a writable data directory for `serve`

## Build and run

```bash
go build -o columna .
./columna serve --data ./data --host 127.0.0.1 --port 18080
# Columna listening on http://127.0.0.1:18080
```

## Command line

```
columna serve  --data <dir> [--host 127.0.0.1] [--port 18080]
columna create --file <path> --schema 'name:type[?],...'
columna write  --file <path> --schema 'name:type[?],...' --rows <jsonl|json> [--append] [--max-page-values n]
columna query  --file <path> [--select a,b] [--where 'a > 1'] [--agg 'sum:b'] [--limit n] [--no-pushdown]
columna stats  --file <path>
```

- `serve` — `--data` defaults to `.`, `--host` to `127.0.0.1`, `--port` to
  `18080`; every `<name>.columna` in the directory is loaded at startup.
- `create` — writes an empty table (zero row groups) and fails if the file
  exists; the parent directory must already exist.
- `write` — `--rows` is a JSON array of objects or JSONL. Without `--append` it
  creates or replaces the file and needs `--schema`; with `--append` it adds one
  row group and takes the schema from the file. `--max-page-values` defaults to
  1024.
- `query` — `--select` is a comma separated projection and `--where`/`--agg`
  repeat. A condition is `column op literal` or `column between lo hi`, where a
  literal is `true`/`false`, an integer, a float, or otherwise a string with
  optional surrounding quotes; `--agg` accepts `count`, `count:col`, `sum:col`,
  `min:col`, `max:col`. `--limit` defaults to unlimited and `--no-pushdown`
  decodes every page. The result is the `POST /query` body.
- `stats` — prints the `GET /tables/{name}/stats` body for a file.

Schema text is `name:type` per column, comma separated, with a trailing `?` for
nullable columns, for example `id:int64,region:string,score:float64?,ok:bool`.
Types are `int64`, `float64`, `bool` and `string`; a column name uses letters,
digits and underscores, starts with a letter or underscore, is at most 64 bytes
and a schema holds 1 to 512 columns.

Exit status is `0` on success, `1` for a runtime error and `2` for a usage error;
every error is written to stderr as `columna: <message>`.

## HTTP API

Bodies are JSON. A `POST` must send `Content-Type: application/json` (parameters
after `;` are accepted) and is limited to 8 MiB. Unknown or missing JSON fields
are rejected instead of ignored. Any route or method outside this list is
`404 not_found`. `GET /health` returns `{"status":"ok"}`.

### POST /tables

```http
{"name":"events","schema":{"columns":[
  {"name":"id","type":"int64"},{"name":"region","type":"string"},
  {"name":"score","type":"float64","nullable":true},
  {"name":"flag","type":"bool"}]}}
```

201 with `{"name":"events","schema":{...},"row_count":0,"row_groups":0}`. An
existing name is `409 conflict`; an invalid name, an empty schema, an unknown
type, a duplicate column or more than 512 columns is `400 validation_error`.

### POST /tables/{name}/rows

```http
{"rows":[{"id":1,"region":"north","score":1.5,"flag":true}],
 "max_page_values":3,"encoding":{"region":"rle"}}
```

`rows` is a non-empty array of objects carrying exactly the table's columns; a
missing value, an unknown field, a wrongly typed value, a `null` in a
non-nullable column, a string with an embedded NUL byte or a non-finite float is
a `400`; at most 50000 rows per request. `max_page_values` must be positive and
`encoding` pins one encoding per column. An unknown table is `404`. Every request
adds exactly one row group. Returns 201:

```json
{"table":"events","appended_rows":1,"row_count":1,"row_group_count":1,
 "row_group_index":0,"file_size":1765,
 "encodings":[{"column":"id","pages":{"dictionary":0,"plain":1,"rle":0}}]}
```

### POST /query

```http
{"table":"events","projection":["id","region"],
 "predicates":[{"column":"id","op":">=","value":5}],
 "limit":100,"pushdown":true,"max_pages_per_group":8}
```

- `table` — table to query, required; `projection` — columns to return in any
  order, defaulting to the whole schema;
- `predicates` — AND of `{"column","op","value"}` or
  `{"column","op":"between","lo","hi"}`;
- `aggregates` — `{"func","column"}`, mutually exclusive with `projection`;
- `limit` — maximum rows returned (`0` returns none); ignored with aggregates;
- `pushdown` — default `true`; `false` decodes every page;
- `max_pages_per_group` — only the first N pages of each column per row group.

`NULL` never satisfies a predicate. `count` without a column counts every
matched row, `count` with a column counts non-NULL values, `min`/`max` ignore
NULLs and return `null` when nothing matched, `sum` accepts `int64`/`float64`
columns, computes in `float64`, ignores NULLs and returns `0` when nothing
matched. Strings support `=` and lexicographic `between`; ordering operators on
a string column are rejected. An `int64` column accepts an integral float
literal and a `float64` column an integer literal; a `bool` column orders
`false` before `true`.

```json
{"columns":["id","region"],
 "rows":[{"id":5,"region":"south"},{"id":7,"region":"north"}],
 "row_count":2,"matched_rows":2,"scanned_rows":7,"scanned_row_groups":2,
 "scanned_pages":9,"skipped_pages":1,"pushdown":true}
```

`columns` is the projection order and `rows` follows row group order, then row
order, so responses are deterministic. `matched_rows` counts the selected rows
(the scan stops once `limit` rows are collected), `scanned_rows` the rows
examined, `scanned_pages` the pages decoded and `skipped_pages` the pages pruning
removed. An aggregate query returns exactly one row keyed by `<func>_<column>`,
or `<func>` for `count` over every row, with a numeric suffix for a repeat.

### GET /tables/{name}/stats

```json
{"table":"events","schema":{...},
 "stats":{"columns":[{"name":"id","type":"int64","pages":5,"row_count":12,
   "null_count":0,"min":1,"max":12}],"row_count":12,"row_group_count":2,
   "file_size":2129,"encodings":["plain","dictionary"]},
 "checksum":"62b4fa0b388c64f2180ec355f867813ba97871e0ed09c9da6f753117544ecbbd"}
```

`min`/`max` are `null` for a column without a non-NULL value, `encodings` lists
the encodings present in the file and `checksum` is the SHA-256 stored in the
file trailer. An unknown table is `404`.

### Idempotency and errors

`Idempotency-Key` on `POST /tables` and `POST /tables/{name}/rows` is stored in
`<data-dir>/.idempotency.json` and survives a restart. Replaying the same request
with the same key returns the recorded status and body without writing again;
reusing a key for another operation or body is `409 idempotency_conflict`. Errors
look like `{"error":{"code":"validation_error","message":"unknown field"}}`,
with `validation_error` and `format_error` mapped to 400, `not_found` to 404,
`conflict` and `idempotency_conflict` to 409 and `internal_error` to 500.

## On-disk format

A file is the header, the data area, the footer, the trailer, the footer magic
and a checksum; all integers are big endian unless stated otherwise and a file is
at least 72 bytes. The header is the magic `COLUMNA1` (8 bytes), a `u16` format
version (currently `1`) and a `u16` column count; the data area follows, holding
every row group's page bytes in write order. The footer follows the data area.
The trailer is 20 bytes: `u32` footer length, `u64` footer offset, `u32` column
count and `u32` row group count. It is followed by the magic `COLUMNAF` (8 bytes)
and a 32-byte SHA-256 checksum over every byte before it.

The header, trailer and footer counts must agree or the file is rejected as a `format_error`; files are replaced atomically (write, parse, rename), so a failed write never leaves a partially updated table behind.

The data area stores each row group as the pages of the schema columns in schema
order, page index 0 first. A page starts with its own header: `u32` page index,
`u32` value count, `u8` encoding code (`0` plain, `1` dictionary, `2` rle), the
encoding's extra fields (`u32` size plus `u8` index width for dictionary, `u32`
run count for rle), `u64` payload length and the payload.

The footer is: (1) `u32` version and `u16` column count; (2) per column a `u16`
name length, the name bytes, a `u8` type code (`1` int64, `2` float64, `3` bool,
`4` string) and a `u8` nullable flag; (3) `u32` row group count; (4) per row
group a `u64` row count, `u32` page capacity, `u32` column count, then per column
a `u32` page count followed by one entry per page: `u32` page index, `u32` value
count, `u8` encoding code, the encoding's extra fields exactly as in the page
header, `u64` absolute page offset, `u64` page length (header included), `u64`
payload length, `u64` null count, minimum statistic and maximum statistic;
(5) per column a `u16` name length, the name bytes, a `u8` type code, `u64` page
count, `u64` row count, `u64` null count, minimum statistic and maximum
statistic.

A statistic is `u8 0` when absent, otherwise `u8 1` followed by the value in
plain layout: 8 bytes little endian for `int64`/`float64`, 1 byte for `bool`, and
for `string` a `u16` length including the trailing NUL byte followed by the
bytes. A string boundary longer than 65534 bytes, and both boundaries of an
all-NULL page, are recorded as absent, which only makes pruning more
conservative.

## Page encodings

Every payload starts with a NULL bitmap of `ceil(value count / 8)` bytes, where
bit `i` (LSB first inside the byte) marks row `i` as NULL. The rest encodes the
non-NULL values in row order:

- `plain` — `int64`/`float64` as 8 little-endian bytes, `bool` as one `0`/`1`
  byte, `string` as UTF-8 bytes plus a NUL terminator;
- `dictionary` — the distinct values in order of first appearance, each in plain
  layout, then the value indices packed LSB first into
  `ceil(log2(dictionary size))` bits each (no index bytes for a one-entry
  dictionary);
- `rle` — one LEB128 varint run length per run, then one plain-layout value per
  run in run order; lengths are positive and sum to the non-NULL value count.

RLE requires the non-NULL values to form one contiguous block, so it rejects a
page whose NULLs sit between values; plain and dictionary accept NULLs anywhere.
When an encoding is not pinned, the writer encodes each page with all candidates,
picks the smallest payload and breaks ties in favour of dictionary, then rle,
then plain; dictionary is only considered when a value repeats and rle only when
the non-NULL values are non-decreasing. The choice is deterministic, so equal
rows always produce equal files.

## Predicate pushdown

Each page records the minimum, the maximum and the NULL count of its non-NULL
values. Before decoding a page the engine asks whether any row in it can satisfy
every predicate on that column, and skips the page only on a provable "no":

| predicate | page skipped when |
| --- | --- |
| any | every value in the page is NULL |
| `col = v` | `v` is outside `[min, max]` |
| `col < v` / `col <= v` | `min >= v` / `min > v` |
| `col > v` / `col >= v` | `max <= v` / `max < v` |
| `col between lo hi` | `max < lo` or `min > hi` |

Pushdown cannot change the result: predicates are evaluated only against decoded
values, a row is evaluated only when every column the query reads has a decoded
value for it, and a skipped page holds no row that the table above admits. So
`rows` is identical with `pushdown` on and off, and only the counters differ.
`max_pages_per_group` applies by page index in both modes and so leaves the rows
unchanged too.

## Scan cost estimation

Before reading any data, a caller can ask which of several files or shards is
cheapest to scan. The estimator is metadata only — `EstimateScanCost` never
opens a page payload and never writes or rewrites a file — and it is opt in:
nothing in the query path changes unless it is called explicitly.

```go
result, err := columna.EstimateScanCost(columna.ScanRequest{
    Sources:       candidates,        // candidate files or shards
    ReadColumns:   []string{"id", "region"},
    ProjectionColumns: []string{"region"},
    Predicates:    []columna.CostPredicate{...},
    Limit:         &limit,            // nil means unlimited
    SortRows:      0,                 // rows a sort must consume first
})
result.Selected      // cheapest candidate; "" only when limit is zero
result.Candidates[i] // estimate of candidate i, in input order
```

Each `CandidateSource` carries its row count, its row group and page
boundaries, per-column per-page byte counts, null counts, distinct counts and
the available min/max values. Every estimate reports, in input order, the row
groups and pages expected to be read, the bytes and values decoded, the rows
left after filtering, the returned-row cap, a human-readable reason, whether
the statistics were complete and one unitless cost:

```
cost = bytes / 1000 + 0.01 * decoded_values + 0.001 * processed_rows
```

Estimation first drops columns that are neither projected nor filtered, then
prunes row groups and pages whose statistics make a predicate hit impossible
(reusing the same rules as predicate pushdown), and then sizes the remainder.
Selectivity follows fixed rules:

| predicate | selectivity |
| --- | --- |
| `col = v` | non-null fraction ÷ distinct count |
| range / `between` | overlap of the predicate interval with `[min, max]`, times the non-null fraction (the non-null fraction for strings when the intervals overlap) |
| `IS NULL` / `IS NOT NULL` | null fraction / non-null fraction |

A positive `limit` lets the scan stop early at page granularity; `SortRows` is
how many filtered rows a sort must consume before that stop can happen. The
returned-row cap is `min(filtered rows, limit)`; limit zero returns zero-cost
estimates and selects no candidate. Missing statistics are never treated as
zero cost: the selectivity defaults to one and `incomplete_statistics` is set.
Equal input and equal metadata always produce an equal result, and a cost tie
selects the first input candidate.

The footer already stores everything except a distinct value count, so
`ReadFileMetadata` parses a file's footer alone and `(*FileMetadata).Candidate`
(or `TableCandidate` for a loaded table) builds a candidate from it, passing a
zero distinct count; equality estimates built this way are flagged
`incomplete_statistics` rather than under-costed.

Invalid input — an empty candidate list, negative row/page/byte/null counts,
nulls above a page's row count, a max below its min, or distinct values above
its non-null row count — returns `InvalidArgumentError`. A predicate type or
operator the model cannot price returns `UnsupportedPredicateError`, checked
up front so no partial estimate is ever returned.

## Tests

```bash
gofmt -l .    # must print nothing
go vet ./...
go test ./...
```

`tests/format_test.go` covers the codecs, page layouts and footer statistics;
`tests/engine_test.go` the query engine, the append path and all-NULL pages.
