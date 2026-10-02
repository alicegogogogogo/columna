package columna

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
)

// On-disk constants. The byte layout is documented in README.md.
var Magic = [8]byte{'C', 'O', 'L', 'U', 'M', 'N', 'A', '1'}

const (
	FormatVersion = 1
	FooterMagic   = "COLUMNAF"

	// The footer ends with a 12-byte trailer holding the footer length, the
	// footer's absolute offset and the column and row group counts.
	FooterTrailerSize = 4 + 8 + 4 + 4
	// The file ends with the footer magic followed by the SHA-256 checksum of
	// every byte before the magic.
	ChecksumSize = 32
	// FileHeaderSize is the fixed prologue: the 8 magic bytes, the 2-byte
	// format version and the 2-byte column count. The data area follows it.
	FileHeaderSize = 8 + 2 + 2
	DefaultMaxPage = 1024
)

// PageHeader describes one encoded column page. Offset is the absolute byte
// offset of the page (starting at its 4-byte page index) and Length is the
// total page byte length including the header.
type PageHeader struct {
	PageIndex  uint32
	ValueCount uint32
	Encoding   string
	DictSize   uint32
	RunCount   uint32
	IndexBits  uint8
	Offset     uint64
	Length     uint64
	Payload    uint64 // payload byte length (Length minus the page header)

	// ColumnType and Path are filled in when the header is attached to a
	// parsed file; they are not stored on disk.
	ColumnType string
	Path       string

	// stats is the recorded min/max/null summary of the page, derived from
	// the footer when a file is parsed.
	stats PageStats
}

// ColumnStats is the footer summary of one column.
type ColumnStats struct {
	Name      string
	Type      string
	Pages     int
	RowCount  int64
	NullCount int64
	Min       Value
	Max       Value
}

func (c ColumnStats) JSON() map[string]any {
	out := map[string]any{
		"name":       c.Name,
		"type":       c.Type,
		"pages":      c.Pages,
		"row_count":  c.RowCount,
		"null_count": c.NullCount,
	}
	if c.NullCount == c.RowCount {
		out["min"] = nil
		out["max"] = nil
		return out
	}
	out["min"] = c.Min.JSON()
	out["max"] = c.Max.JSON()
	return out
}

// MarshalJSON renders a column summary in the same shape as JSON, so API
// responses never expose the internal Value representation.
func (c ColumnStats) MarshalJSON() ([]byte, error) { return json.Marshal(c.JSON()) }

// FileStats is exposed by GET /tables/{name}/stats.
type FileStats struct {
	Columns       []ColumnStats `json:"columns"`
	RowCount      int64         `json:"row_count"`
	RowGroupCount int           `json:"row_group_count"`
	FileSize      int64         `json:"file_size"`
	Encodings     []string      `json:"encodings"`
}

// RowGroup is one written batch of rows. Pages holds the footer page table;
// raw holds the encoded page bytes of a row group built in this process.
type RowGroup struct {
	RowCount int
	// PageValues is the page capacity this row group was written with: every
	// page except the last holds exactly this many values.
	PageValues int
	Pages      map[string][]PageHeader
	raw        []byte
}

// File is the in-memory view of a Columna file.
type File struct {
	path      string
	schema    Schema
	stats     FileStats
	rowGroups []RowGroup
}

// Path returns the file path this view was read from.
func (f *File) Path() string { return f.path }

// Schema returns the schema recorded in the footer.
func (f *File) Schema() Schema { return f.schema }

// Stats returns the footer statistics.
func (f *File) Stats() FileStats { return f.stats }

// RowGroups returns the row groups in write order.
func (f *File) RowGroups() []RowGroup { return f.rowGroups }

// ColumnValues decodes every value of one column, in row order.
func (f *File) ColumnValues(name string) ([]Value, error) {
	if _, ok := f.schema.Column(name); !ok {
		return nil, Validationf("unknown column %q", name)
	}
	var out []Value
	for _, group := range f.rowGroups {
		for _, header := range group.Pages[name] {
			values, err := f.pageValues(header)
			if err != nil {
				return nil, err
			}
			out = append(out, values...)
		}
	}
	return out, nil
}

type fileWriter struct{ buf bytes.Buffer }

func (w *fileWriter) u8(v uint8)   { w.buf.WriteByte(v) }
func (w *fileWriter) u16(v uint16) { binary.Write(&w.buf, binary.BigEndian, v) }
func (w *fileWriter) u32(v uint32) { binary.Write(&w.buf, binary.BigEndian, v) }
func (w *fileWriter) u64(v uint64) { binary.Write(&w.buf, binary.BigEndian, v) }
func (w *fileWriter) raw(b []byte) { w.buf.Write(b) }

func (w *fileWriter) str(s string) {
	w.u16(uint16(len(s)))
	w.raw([]byte(s))
}

func encodingCode(enc string) uint8 {
	switch enc {
	case EncodingPlain:
		return 0
	case EncodingDictionary:
		return 1
	case EncodingRLE:
		return 2
	}
	return 255
}

func encodingName(code uint8) (string, error) {
	switch code {
	case 0:
		return EncodingPlain, nil
	case 1:
		return EncodingDictionary, nil
	case 2:
		return EncodingRLE, nil
	}
	return "", Formatf("unknown encoding code %d", code)
}

func typeCode(t string) uint8 {
	switch t {
	case TypeInt64:
		return 1
	case TypeFloat64:
		return 2
	case TypeBool:
		return 3
	case TypeString:
		return 4
	}
	return 0
}

func typeName(code uint8) (string, error) {
	switch code {
	case 1:
		return TypeInt64, nil
	case 2:
		return TypeFloat64, nil
	case 3:
		return TypeBool, nil
	case 4:
		return TypeString, nil
	}
	return "", Formatf("unknown column type code %d", code)
}

// encodeColumn splits one column of a row group into pages, choosing the
// smallest legal encoding per page unless an encoding was forced.
func encodeColumn(typ string, values []Value, maxPerPage int, forced string) ([]PageHeader, [][]byte, PageStats, error) {
	if maxPerPage <= 0 {
		maxPerPage = DefaultMaxPage
	}
	var headers []PageHeader
	var bodies [][]byte
	groupStats := PageStats{Min: NullValue(), Max: NullValue()}
	for start, index := 0, uint32(0); start < len(values); index++ {
		end := start + maxPerPage
		if end > len(values) {
			end = len(values)
		}
		chunk := values[start:end]
		encoding := forced
		if encoding == "" {
			encoding = chooseEncoding(chunk, typ)
		}
		payload, stats, err := encodePage(chunk, typ, encoding)
		if err != nil {
			return nil, nil, PageStats{}, err
		}
		header := PageHeader{
			PageIndex:  index,
			ValueCount: uint32(len(chunk)),
			Encoding:   payload.Encoding,
			DictSize:   payload.DictSize,
			RunCount:   payload.RunCount,
			IndexBits:  payload.IndexBits,
			Payload:    uint64(len(payload.Payload)),
			stats:      stats,
		}
		bodies = append(bodies, buildPageBytes(header, payload.Payload))
		headers = append(headers, header)
		groupStats.Nulls += stats.Nulls
		if stats.Set {
			if !groupStats.Set {
				groupStats.Set, groupStats.Min, groupStats.Max = true, stats.Min, stats.Max
			} else {
				if less(stats.Min, groupStats.Min) {
					groupStats.Min = stats.Min
				}
				if less(groupStats.Max, stats.Max) {
					groupStats.Max = stats.Max
				}
			}
		}
		start = end
	}
	return headers, bodies, groupStats, nil
}

func buildPageBytes(h PageHeader, payload []byte) []byte {
	w := &fileWriter{}
	w.u32(h.PageIndex)
	w.u32(h.ValueCount)
	w.u8(encodingCode(h.Encoding))
	switch h.Encoding {
	case EncodingDictionary:
		w.u32(h.DictSize)
		w.u8(h.IndexBits)
	case EncodingRLE:
		w.u32(h.RunCount)
	}
	w.u64(uint64(len(payload)))
	w.raw(payload)
	return w.buf.Bytes()
}

func chooseEncoding(values []Value, typ string) string {
	if len(values) == 0 {
		return EncodingPlain
	}
	nonNull := nonNullIndices(values)
	costs := map[string]int{}
	plain, _, err := encodePage(values, typ, EncodingPlain)
	if err != nil {
		return EncodingPlain
	}
	costs[EncodingPlain] = len(plain.Payload)
	if DistinctCount(values) < len(nonNull) {
		if dict, _, err := encodePage(values, typ, EncodingDictionary); err == nil {
			costs[EncodingDictionary] = len(dict.Payload)
		}
	}
	if SortedForRLE(values) {
		if rle, _, err := encodePage(values, typ, EncodingRLE); err == nil {
			costs[EncodingRLE] = len(rle.Payload)
		}
	}
	best := EncodingPlain
	// The cheapest payload wins; ties are broken deterministically in favour of
	// dictionary, then rle, then plain.
	for _, candidate := range []string{EncodingRLE, EncodingDictionary} {
		if cost, ok := costs[candidate]; ok && cost <= costs[best] {
			best = candidate
		}
	}
	return best
}

// WriteFile writes a complete Columna file. forced optionally pins the
// encoding of individual columns.
func WriteFile(path string, schema Schema, groups [][]Row, maxPerPage int, forced map[string]string) (*File, error) {
	rowGroups, columnStats, err := buildRowGroups(schema, groups, maxPerPage, forced)
	if err != nil {
		return nil, err
	}
	raw, err := renderFile(schema, rowGroups, columnStats, nil)
	if err != nil {
		return nil, err
	}
	if err := writeAtomically(path, raw); err != nil {
		return nil, err
	}
	return ReadFile(path)
}

// buildRowGroups encodes the given batches of rows. Page offsets are relative
// to the start of the row group data area.
func buildRowGroups(schema Schema, groups [][]Row, maxPerPage int, forced map[string]string) ([]RowGroup, []ColumnStats, error) {
	if err := schema.Validate(); err != nil {
		return nil, nil, err
	}
	rowGroups := make([]RowGroup, 0, len(groups))
	columnStats := make([]ColumnStats, len(schema.Columns))
	for i, c := range schema.Columns {
		columnStats[i] = ColumnStats{Name: c.Name, Type: c.Type, Min: NullValue(), Max: NullValue()}
	}
	for _, rows := range groups {
		group := RowGroup{RowCount: len(rows), PageValues: normalizedPageWidth(maxPerPage), Pages: map[string][]PageHeader{}}
		for columnIndex, column := range schema.Columns {
			values := make([]Value, len(rows))
			for i, row := range rows {
				if len(row) != len(schema.Columns) {
					return nil, nil, Validationf("row %d has %d values, expected %d", i, len(row), len(schema.Columns))
				}
				if row[columnIndex].Null && !column.Nullable {
					return nil, nil, Validationf("column %q is not nullable", column.Name)
				}
				values[i] = row[columnIndex]
			}
			headers, bodies, stats, err := encodeColumn(column.Type, values, maxPerPage, forced[column.Name])
			if err != nil {
				return nil, nil, err
			}
			// Page offsets stay relative to the row group; renderFile adds
			// the row group base offset when it lays the data area out.
			for i := range headers {
				headers[i].Offset = uint64(len(group.raw))
				headers[i].Length = uint64(len(bodies[i]))
				group.raw = append(group.raw, bodies[i]...)
			}
			group.Pages[column.Name] = headers
			entry := &columnStats[columnIndex]
			entry.Pages += len(headers)
			entry.RowCount += int64(len(rows))
			entry.NullCount += stats.Nulls
			if stats.Set {
				if !entry.Min.Null && entry.Min.Kind == "" {
					return nil, nil, errorf(CodeInternal, "column statistics were not initialised")
				}
				if entry.Min.Null {
					entry.Min, entry.Max = stats.Min, stats.Max
				} else {
					if less(stats.Min, entry.Min) {
						entry.Min = stats.Min
					}
					if less(entry.Max, stats.Max) {
						entry.Max = stats.Max
					}
				}
			}
		}
		rowGroups = append(rowGroups, group)
	}
	return rowGroups, columnStats, nil
}

// renderFile serialises a whole Columna file: header, every row group's pages,
// the footer, the trailer and the payload checksum. Page offsets held in memory
// are relative to their row group; the footer records absolute offsets. data is
// the data area that the leading row groups must reproduce byte for byte: the
// append path passes the original file's data area, so a mis-copied or
// mis-rebased row group is rejected instead of silently corrupting the file.
// Pass nil when the file is written from scratch.
func renderFile(schema Schema, groups []RowGroup, columnStats []ColumnStats, data []byte) ([]byte, error) {
	w := &fileWriter{}
	w.raw(Magic[:])
	w.u16(FormatVersion)
	w.u16(uint16(len(schema.Columns)))
	for _, group := range groups {
		w.raw(group.raw)
	}
	if data != nil {
		written := w.buf.Bytes()[FileHeaderSize:]
		if len(written) < len(data) || !bytes.Equal(written[:len(data)], data) {
			return nil, errorf(CodeInternal, "row group bytes do not match the recorded data area (%d vs %d)", len(written), len(data))
		}
	}
	laid := make([]RowGroup, 0, len(groups))
	base := uint64(FileHeaderSize)
	for _, group := range groups {
		copied := RowGroup{RowCount: group.RowCount, PageValues: group.PageValues, Pages: map[string][]PageHeader{}}
		for name, headers := range group.Pages {
			shifted := make([]PageHeader, len(headers))
			for i, h := range headers {
				// In-memory headers carry offsets relative to the start of
				// their row group; the footer records absolute offsets.
				h.Offset += base
				shifted[i] = h
			}
			copied.Pages[name] = shifted
		}
		laid = append(laid, copied)
		base += uint64(len(group.raw))
	}
	footerOffset := w.buf.Len()
	footer := buildFooter(schema, laid, columnStats)
	w.raw(footer)
	w.u32(uint32(len(footer)))
	w.u64(uint64(footerOffset))
	w.u32(uint32(len(schema.Columns)))
	w.u32(uint32(len(laid)))
	w.raw([]byte(FooterMagic))
	sum := sha256.Sum256(w.buf.Bytes())
	w.raw(sum[:])
	return w.buf.Bytes(), nil
}

func normalizedPageWidth(maxPerPage int) int {
	if maxPerPage <= 0 {
		return DefaultMaxPage
	}
	return maxPerPage
}

// dataOrder lists the schema columns so the data area order is deterministic.
func dataOrder(schema Schema) []string {
	names := make([]string, len(schema.Columns))
	for i, c := range schema.Columns {
		names[i] = c.Name
	}
	return names
}

// writeAtomically writes raw next to path and renames it over the target only
// after the temporary file parses, so a file that fails validation never
// replaces a readable one.
func writeAtomically(path string, raw []byte) error {
	temp := path + ".tmp"
	if err := os.WriteFile(temp, raw, 0o644); err != nil {
		return errorf(CodeInternal, "cannot write %s: %v", temp, err)
	}
	if _, err := ReadFile(temp); err != nil {
		os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return errorf(CodeInternal, "cannot replace %s: %v", path, err)
	}
	return nil
}

// AppendRows rewrites path so that it holds the existing row groups followed
// by one new row group. The result is byte-identical to writing every batch in
// a single call.
func AppendRows(path string, schema Schema, rows []Row, maxPerPage int, forced map[string]string) (*File, error) {
	existing, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !schema.Equal(existing.schema) {
		return nil, Validationf("rows do not match the schema of %s", path)
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return nil, errorf(CodeInternal, "cannot read %s: %v", path, err)
	}
	trailerAt := len(old) - ChecksumSize - FooterMagicSize - FooterTrailerSize
	if trailerAt < 0 || string(old[trailerAt+FooterTrailerSize:trailerAt+FooterTrailerSize+FooterMagicSize]) != FooterMagic {
		return nil, Formatf("file %s has no footer magic", path)
	}
	footerOffset := int(binary.BigEndian.Uint64(old[trailerAt+4 : trailerAt+12]))

	newGroups, newStats, err := buildRowGroups(schema, [][]Row{rows}, maxPerPage, forced)
	if err != nil {
		return nil, err
	}
	combinedStats := append([]ColumnStats{}, existing.stats.Columns...)
	for i := range combinedStats {
		combinedStats[i].Pages += newStats[i].Pages
		combinedStats[i].NullCount += newStats[i].NullCount
		combinedStats[i].RowCount += newStats[i].RowCount
		if newStats[i].Min.Null {
			continue
		}
		if combinedStats[i].Min.Null {
			combinedStats[i].Min, combinedStats[i].Max = newStats[i].Min, newStats[i].Max
			continue
		}
		if less(newStats[i].Min, combinedStats[i].Min) {
			combinedStats[i].Min = newStats[i].Min
		}
		if less(combinedStats[i].Max, newStats[i].Max) {
			combinedStats[i].Max = newStats[i].Max
		}
	}
	combined := make([]RowGroup, 0, len(existing.rowGroups)+len(newGroups))
	for _, group := range existing.rowGroups {
		raw, base, err := slicesOf(old, existing.schema, group)
		if err != nil {
			return nil, err
		}
		rebuilt, err := rebasedGroup(group, base, raw)
		if err != nil {
			return nil, err
		}
		combined = append(combined, rebuilt)
	}
	combined = append(combined, newGroups...)
	// The leading row groups are byte-for-byte copies of the old data area, so
	// re-rendering them must reproduce that area exactly. renderFile enforces
	// it; the header prologue is not part of the data area.
	out, err := renderFile(existing.schema, combined, combinedStats, old[FileHeaderSize:footerOffset])
	if err != nil {
		return nil, err
	}
	if err := writeAtomically(path, out); err != nil {
		return nil, err
	}
	return ReadFile(path)
}

// slicesOf returns the encoded data area of an existing row group so the append
// path can copy it verbatim, together with the absolute file offset the area
// starts at. The data area is the contiguous byte range that covers every page
// of every column of the group.
func slicesOf(raw []byte, schema Schema, group RowGroup) ([]byte, uint64, error) {
	width := normalizedPageWidth(group.PageValues)
	start := uint64(len(raw))
	end := uint64(0)
	for _, name := range dataOrder(schema) {
		headers := group.Pages[name]
		for p, header := range headers {
			if int(header.PageIndex) != p {
				return nil, 0, Formatf("column %q page %d breaks the page sequence", name, header.PageIndex)
			}
			if p < len(headers)-1 && int(header.ValueCount) != width {
				return nil, 0, Formatf("column %q page %d holds %d values, expected %d", name, header.PageIndex, header.ValueCount, width)
			}
			if header.Offset+header.Length > uint64(len(raw)) {
				return nil, 0, Formatf("column %q page %d overruns the data area", name, header.PageIndex)
			}
			if header.Offset < start {
				start = header.Offset
			}
			if header.Offset+header.Length > end {
				end = header.Offset + header.Length
			}
		}
	}
	if end <= start {
		return nil, 0, nil
	}
	return append([]byte{}, raw[start:end]...), start, nil
}

// rebasedGroup converts a row group parsed from a file into the in-memory write
// representation: the encoded page bytes are kept verbatim and the absolute
// offsets recorded in the footer are turned back into offsets relative to the
// start of the row group, which is what renderFile lays out.
func rebasedGroup(group RowGroup, base uint64, raw []byte) (RowGroup, error) {
	out := RowGroup{
		RowCount:   group.RowCount,
		PageValues: group.PageValues,
		Pages:      make(map[string][]PageHeader, len(group.Pages)),
		raw:        raw,
	}
	for name, headers := range group.Pages {
		shifted := make([]PageHeader, len(headers))
		for i, header := range headers {
			if header.Offset < base || header.Offset+header.Length > base+uint64(len(raw)) {
				return RowGroup{}, Formatf("column %q page %d lies outside its row group", name, header.PageIndex)
			}
			header.Offset -= base
			shifted[i] = header
		}
		out.Pages[name] = shifted
	}
	return out, nil
}

// buildFooter serialises the footer: version, schema, page tables and column
// statistics. Every page table entry carries the page's encoding header and
// its recorded statistics, so a reader never has to touch a page header.
func buildFooter(schema Schema, groups []RowGroup, stats []ColumnStats) []byte {
	w := &fileWriter{}
	w.u32(FormatVersion)
	w.u16(uint16(len(schema.Columns)))
	for _, c := range schema.Columns {
		w.str(c.Name)
		w.u8(typeCode(c.Type))
		flag := uint8(0)
		if c.Nullable {
			flag = 1
		}
		w.u8(flag)
	}
	w.u32(uint32(len(groups)))
	for _, group := range groups {
		w.u64(uint64(group.RowCount))
		w.u32(uint32(normalizedPageWidth(group.PageValues)))
		w.u32(uint32(len(schema.Columns)))
		for _, column := range schema.Columns {
			headers := group.Pages[column.Name]
			w.u32(uint32(len(headers)))
			for _, h := range headers {
				w.u32(h.PageIndex)
				w.u32(h.ValueCount)
				w.u8(encodingCode(h.Encoding))
				switch h.Encoding {
				case EncodingDictionary:
					w.u32(h.DictSize)
					w.u8(h.IndexBits)
				case EncodingRLE:
					w.u32(h.RunCount)
				}
				w.u64(h.Offset)
				w.u64(h.Length)
				w.u64(h.Payload)
				w.u64(uint64(h.stats.Nulls))
				encodeStatValue(w, h.stats.Min)
				encodeStatValue(w, h.stats.Max)
			}
		}
	}
	for _, entry := range stats {
		w.str(entry.Name)
		w.u8(typeCode(entry.Type))
		w.u64(uint64(entry.Pages))
		w.u64(uint64(entry.RowCount))
		w.u64(uint64(entry.NullCount))
		encodeStatValue(w, entry.Min)
		encodeStatValue(w, entry.Max)
	}
	return w.buf.Bytes()
}

// encodeStatValue writes one boundary statistic: an absence flag followed by
// the value in plain layout. Strings are prefixed with a u16 length that
// includes their NUL terminator because their plain layout is variable sized;
// int64, float64 and bool values are fixed sized. A string longer than the
// u16 length can express is recorded as absent, which only ever makes the
// statistic more conservative.
func encodeStatValue(w *fileWriter, v Value) {
	if v.Null || v.Kind == "" || (v.Kind == TypeString && len(v.Str)+1 > 0xffff) {
		w.u8(0)
		return
	}
	w.u8(1)
	if v.Kind == TypeString {
		w.u16(uint16(len(v.Str) + 1))
	}
	w.raw(encodePlainValues([]Value{v}, []int{0}))
}

// ReadFile parses a Columna file, validating magic, version, footer and the
// payload checksum.
func ReadFile(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errorf(CodeNotFound, "table %s does not exist", path)
	}
	if len(raw) < 12+FooterMagicSize+FooterTrailerSize+ChecksumSize {
		return nil, Formatf("file %s is too small to be a Columna file", path)
	}
	if !bytes.Equal(raw[:8], Magic[:]) {
		return nil, Formatf("file %s has a bad magic number", path)
	}
	if version := binary.BigEndian.Uint16(raw[8:10]); version != FormatVersion {
		return nil, Formatf("file %s uses unsupported format version %d", path, version)
	}
	want := sha256.Sum256(raw[:len(raw)-ChecksumSize])
	if !bytes.Equal(want[:], raw[len(raw)-ChecksumSize:]) {
		return nil, Formatf("file %s failed its payload checksum", path)
	}
	trailerAt := len(raw) - ChecksumSize - FooterMagicSize - FooterTrailerSize
	trailer := raw[trailerAt : trailerAt+FooterTrailerSize]
	if string(raw[trailerAt+FooterTrailerSize:len(raw)-ChecksumSize]) != FooterMagic {
		return nil, Formatf("file %s has no footer magic", path)
	}
	footerLength := int(binary.BigEndian.Uint32(trailer[0:4]))
	footerOffset := int(binary.BigEndian.Uint64(trailer[4:12]))
	columnCount := int(binary.BigEndian.Uint32(trailer[12:16]))
	groupCount := int(binary.BigEndian.Uint32(trailer[16:20]))
	if footerOffset < 0 || footerOffset+footerLength != trailerAt {
		return nil, Formatf("file %s footer offsets do not add up to the file size", path)
	}
	parsed, err := parseFooter(raw[footerOffset : footerOffset+footerLength])
	if err != nil {
		return nil, err
	}
	parsed.path = path
	parsed.stats.FileSize = int64(len(raw))
	if columnCount != len(parsed.schema.Columns) || groupCount != len(parsed.rowGroups) {
		return nil, Formatf("file %s trailer disagrees with its footer", path)
	}
	rowTotal := int64(0)
	encodings := map[string]bool{}
	for g := range parsed.rowGroups {
		rowTotal += int64(parsed.rowGroups[g].RowCount)
		width := normalizedPageWidth(parsed.rowGroups[g].PageValues)
		for _, column := range parsed.schema.Columns {
			headers := parsed.rowGroups[g].Pages[column.Name]
			for i := range headers {
				if int(headers[i].PageIndex) != i {
					return nil, Formatf("row group %d column %s page %d breaks the page sequence", g, column.Name, headers[i].PageIndex)
				}
				if i < len(headers)-1 && int(headers[i].ValueCount) != width {
					return nil, Formatf("row group %d column %s page %d holds %d values, expected %d", g, column.Name, headers[i].PageIndex, headers[i].ValueCount, width)
				}
				if headers[i].Offset+headers[i].Length > uint64(footerOffset) {
					return nil, Formatf("row group %d column %s page %d overruns the data area", g, column.Name, headers[i].PageIndex)
				}
				headers[i].ColumnType = column.Type
				headers[i].Path = path
				encodings[headers[i].Encoding] = true
			}
			parsed.rowGroups[g].Pages[column.Name] = headers
		}
	}
	parsed.stats.RowCount = rowTotal
	parsed.stats.RowGroupCount = len(parsed.rowGroups)
	parsed.stats.Encodings = []string{}
	for _, candidate := range []string{EncodingPlain, EncodingDictionary, EncodingRLE} {
		if encodings[candidate] {
			parsed.stats.Encodings = append(parsed.stats.Encodings, candidate)
		}
	}
	return parsed, nil
}

const FooterMagicSize = len(FooterMagic)

func parseFooter(raw []byte) (*File, error) {
	r := &footerReader{raw: raw}
	if version := r.u32(); version != FormatVersion {
		return nil, Formatf("footer version %d is unsupported", version)
	}
	columnCount := int(r.u16())
	if r.err != nil {
		return nil, Formatf("footer is truncated")
	}
	if columnCount <= 0 {
		return nil, Formatf("footer declares %d columns", columnCount)
	}
	schema := Schema{}
	for i := 0; i < columnCount; i++ {
		name := r.str()
		code := r.u8()
		typ, err := typeName(code)
		if err != nil {
			return nil, err
		}
		nullable := r.u8() == 1
		schema.Columns = append(schema.Columns, Column{Name: name, Type: typ, Nullable: nullable})
	}
	if r.err != nil {
		return nil, Formatf("footer schema block is truncated")
	}
	groupCount := int(r.u32())
	groups := make([]RowGroup, 0, groupCount)
	for g := 0; g < groupCount; g++ {
		rowCount := int(r.u64())
		pageValues := normalizedPageWidth(int(r.u32()))
		columns := int(r.u32())
		if columns != len(schema.Columns) {
			return nil, Formatf("row group %d lists %d columns, expected %d", g, columns, len(schema.Columns))
		}
		group := RowGroup{RowCount: rowCount, PageValues: pageValues, Pages: map[string][]PageHeader{}}
		for c := 0; c < len(schema.Columns); c++ {
			column := schema.Columns[c]
			pageCount := int(r.u32())
			headers := make([]PageHeader, 0, pageCount)
			for p := 0; p < pageCount; p++ {
				header := PageHeader{}
				header.PageIndex = r.u32()
				header.ValueCount = r.u32()
				encoding, err := encodingName(r.u8())
				if err != nil {
					return nil, err
				}
				header.Encoding = encoding
				switch encoding {
				case EncodingDictionary:
					header.DictSize = r.u32()
					header.IndexBits = r.u8()
				case EncodingRLE:
					header.RunCount = r.u32()
				}
				header.Offset = r.u64()
				header.Length = r.u64()
				header.Payload = r.u64()
				header.stats.Nulls = int64(r.u64())
				header.stats.Min, err = r.statValue(column.Type)
				if err != nil {
					return nil, err
				}
				header.stats.Max, err = r.statValue(column.Type)
				if err != nil {
					return nil, err
				}
				header.stats.Set = !header.stats.Min.Null && !header.stats.Max.Null
				headers = append(headers, header)
			}
			group.Pages[column.Name] = headers
		}
		groups = append(groups, group)
	}
	stats := make([]ColumnStats, len(schema.Columns))
	for i, column := range schema.Columns {
		name := r.str()
		if name != column.Name {
			return nil, Formatf("statistics block %d is for %q, expected %q", i, name, column.Name)
		}
		code := r.u8()
		typ, err := typeName(code)
		if err != nil {
			return nil, err
		}
		if typ != column.Type {
			return nil, Formatf("statistics for %q disagree about the type", name)
		}
		entry := ColumnStats{Name: name, Type: typ}
		entry.Pages = int(r.u64())
		entry.RowCount = int64(r.u64())
		entry.NullCount = int64(r.u64())
		entry.Min, err = r.statValue(typ)
		if err != nil {
			return nil, err
		}
		entry.Max, err = r.statValue(typ)
		if err != nil {
			return nil, err
		}
		stats[i] = entry
	}
	if r.err != nil {
		return nil, r.err
	}
	return &File{
		schema:    schema,
		rowGroups: groups,
		stats:     FileStats{Columns: stats},
	}, nil
}

type footerReader struct {
	raw []byte
	pos int
	err error
}

func (r *footerReader) take(n int) []byte {
	if r.err != nil {
		return make([]byte, n)
	}
	if r.pos+n > len(r.raw) {
		r.err = Formatf("footer is truncated")
		return make([]byte, n)
	}
	out := r.raw[r.pos : r.pos+n]
	r.pos += n
	return out
}

func (r *footerReader) u8() uint8   { return r.take(1)[0] }
func (r *footerReader) u16() uint16 { return binary.BigEndian.Uint16(r.take(2)) }
func (r *footerReader) u32() uint32 { return binary.BigEndian.Uint32(r.take(4)) }
func (r *footerReader) u64() uint64 { return binary.BigEndian.Uint64(r.take(8)) }

func (r *footerReader) str() string { return string(r.take(int(r.u16()))) }

func (r *footerReader) statValue(typ string) (Value, error) {
	present := r.u8()
	if r.err != nil {
		return NullValue(), r.err
	}
	if present == 0 {
		return NullValue(), nil
	}
	var size int
	switch typ {
	case TypeString:
		// The recorded length includes the NUL terminator of the plain
		// layout, so the decoder sees a complete value.
		size = int(r.u16())
	case TypeBool:
		size = 1
	default:
		size = 8
	}
	body := r.take(size)
	if r.err != nil {
		return NullValue(), r.err
	}
	values, err := decodePlainValues(body, typ, 1)
	if err != nil {
		return NullValue(), err
	}
	return values[0], nil
}

// pageValues reads and decodes the payload of one page.
func (p *File) pageValues(h PageHeader) ([]Value, error) {
	file, err := os.Open(p.path)
	if err != nil {
		return nil, errorf(CodeInternal, "cannot read %s: %v", p.path, err)
	}
	defer file.Close()
	payload := make([]byte, h.Payload)
	payloadOffset := int64(h.Offset) + int64(h.Length) - int64(h.Payload)
	if _, err := file.ReadAt(payload, payloadOffset); err != nil {
		return nil, Formatf("page %d payload is truncated: %v", h.PageIndex, err)
	}
	return decodePage(h, payload, int(h.ValueCount), h.ColumnType)
}

// Checksum returns the hex SHA-256 recorded in the file trailer.
func Checksum(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", errorf(CodeNotFound, "table %s does not exist", path)
	}
	if len(raw) < ChecksumSize {
		return "", Formatf("file %s is too small", path)
	}
	return hex.EncodeToString(raw[len(raw)-ChecksumSize:]), nil
}
