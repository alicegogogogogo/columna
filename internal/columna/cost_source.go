package columna

import (
	"os"
)

// Metadata-only adapters between Columna files and the scan cost estimator.
//
// The on-disk footer records everything the estimator needs except a distinct
// value count, which the format does not store; adapters therefore pass zero
// distinct counts, which the estimator treats as "statistics incomplete" for
// equality predicates rather than as a zero cost. Page byte counts use the
// recorded page length including the page header, exactly as the footer stores
// it. No page payload is ever read by these adapters.

// FileMetadata is a view of a Columna file built from its footer alone.
type FileMetadata struct {
	path   string
	parsed *File
}

// ReadFileMetadata parses the schema, row groups, page tables and column
// statistics from the footer without reading any data page payload.
func ReadFileMetadata(path string) (*FileMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errorf(CodeNotFound, "table %s does not exist", path)
	}
	defer file.Close()
	size, err := file.Seek(0, os.SEEK_END)
	if err != nil {
		return nil, errorf(CodeInternal, "cannot seek %s: %v", path, err)
	}
	tailSize := int64(ChecksumSize + FooterMagicSize + FooterTrailerSize)
	if size < int64(FileHeaderSize)+tailSize {
		return nil, Formatf("file %s is too small to be a Columna file", path)
	}
	tail := make([]byte, tailSize)
	if _, err := file.ReadAt(tail, size-tailSize); err != nil {
		return nil, Formatf("file %s trailer is truncated: %v", path, err)
	}
	trailer := tail[:FooterTrailerSize]
	if string(tail[FooterTrailerSize:FooterTrailerSize+FooterMagicSize]) != FooterMagic {
		return nil, Formatf("file %s has no footer magic", path)
	}
	footerLength := int64(trailerU32(trailer, 0))
	footerOffset := int64(trailerU64(trailer, 4))
	columnCount := int64(trailerU32(trailer, 12))
	groupCount := int64(trailerU32(trailer, 16))
	if footerOffset < int64(FileHeaderSize) || footerOffset+footerLength != size-tailSize {
		return nil, Formatf("file %s footer offsets do not add up to the file size", path)
	}
	footer := make([]byte, footerLength)
	if _, err := file.ReadAt(footer, footerOffset); err != nil {
		return nil, Formatf("file %s footer is truncated: %v", path, err)
	}
	parsed, err := parseFooter(footer)
	if err != nil {
		return nil, err
	}
	parsed.path = path
	if columnCount != int64(len(parsed.schema.Columns)) || groupCount != int64(len(parsed.rowGroups)) {
		return nil, Formatf("file %s trailer disagrees with its footer", path)
	}
	// Page headers parsed from the footer carry encoding, offset and length but
	// no column type; fill the type so the statistics stay self-describing.
	for g := range parsed.rowGroups {
		for _, column := range parsed.schema.Columns {
			headers := parsed.rowGroups[g].Pages[column.Name]
			for i := range headers {
				headers[i].ColumnType = column.Type
				headers[i].Path = path
			}
			parsed.rowGroups[g].Pages[column.Name] = headers
		}
	}
	return &FileMetadata{path: path, parsed: parsed}, nil
}

func trailerU32(b []byte, off int) uint32 {
	return uint32(b[off])<<24 | uint32(b[off+1])<<16 | uint32(b[off+2])<<8 | uint32(b[off+3])
}

func trailerU64(b []byte, off int) uint64 {
	var v uint64
	for i := 0; i < 8; i++ {
		v = v<<8 | uint64(b[off+i])
	}
	return v
}

// Path returns the file the metadata was read from.
func (m *FileMetadata) Path() string { return m.path }

// Schema returns the schema recorded in the footer.
func (m *FileMetadata) Schema() Schema { return m.parsed.schema }

// Stats returns the footer statistics.
func (m *FileMetadata) Stats() FileStats { return m.parsed.stats }

// RowGroups returns the row group page tables in write order.
func (m *FileMetadata) RowGroups() []RowGroup { return m.parsed.rowGroups }

// Candidate builds one estimator candidate for the given read columns. Only
// the columns the scan may read need metadata; the footer byte counts, row
// counts, null counts and min/max statistics are reused verbatim. The distinct
// count is zero because the format does not persist it.
func (m *FileMetadata) Candidate(name string, readColumns []string) (CandidateSource, error) {
	return candidateFromFile(name, m.parsed, readColumns)
}

// TableCandidate builds one estimator candidate from a table already loaded
// for querying. It reads no page payloads.
func TableCandidate(table *Table, readColumns []string) (CandidateSource, error) {
	return candidateFromFile(table.Name, table.parsed, readColumns)
}

func candidateFromFile(name string, parsed *File, readColumns []string) (CandidateSource, error) {
	if name == "" {
		name = parsed.path
	}
	known := map[string]Column{}
	for _, column := range parsed.schema.Columns {
		known[column.Name] = column
	}
	for _, column := range readColumns {
		if _, ok := known[column]; !ok {
			return CandidateSource{}, Validationf("unknown column %q in %s", column, name)
		}
	}
	var rowCount int64
	groups := make([]GroupStat, 0, len(parsed.rowGroups))
	for _, group := range parsed.rowGroups {
		rowCount += int64(group.RowCount)
		pages := map[string][]PageStat{}
		for _, column := range readColumns {
			headers := group.Pages[column]
			entries := make([]PageStat, 0, len(headers))
			for _, header := range headers {
				entries = append(entries, pageStatFromHeader(header))
			}
			pages[column] = entries
		}
		groups = append(groups, GroupStat{Rows: int64(group.RowCount), Pages: pages})
	}

	columns := map[string]ColumnInput{}
	for _, column := range readColumns {
		summary, ok := columnSummary(parsed, column)
		if !ok {
			return CandidateSource{}, Validationf("candidate %q is missing footer statistics for %q", name, column)
		}
		input := ColumnInput{
			Nulls: summary.NullCount,
			Set:   summary.NullCount < summary.RowCount,
			Min:   summary.Min,
			Max:   summary.Max,
		}
		for _, group := range parsed.rowGroups {
			for _, header := range group.Pages[column] {
				input.Bytes = append(input.Bytes, int64(header.Length))
			}
		}
		columns[column] = input
	}
	return CandidateSource{
		Name:    name,
		Rows:    rowCount,
		Groups:  groups,
		Columns: columns,
	}, nil
}

func pageStatFromHeader(header PageHeader) PageStat {
	return PageStat{
		Rows:     int64(header.ValueCount),
		Nulls:    header.stats.Nulls,
		Distinct: 0, // the footer does not persist a distinct value count
		Set:      header.stats.Set,
		Min:      header.stats.Min,
		Max:      header.stats.Max,
	}
}

func columnSummary(parsed *File, column string) (ColumnStats, bool) {
	for _, summary := range parsed.stats.Columns {
		if summary.Name == column {
			return summary, true
		}
	}
	return ColumnStats{}, false
}

// CostPredicateFrom converts a query predicate already bound to a schema into
// an estimator predicate. The column type is taken from the bound value or,
// for completeness, the supplied schema.
func CostPredicateFrom(predicate Predicate, schema Schema) (CostPredicate, error) {
	column, ok := schema.Column(predicate.Column)
	if !ok {
		return CostPredicate{}, Validationf("unknown column %q", predicate.Column)
	}
	out := CostPredicate{Column: predicate.Column, Type: column.Type, Op: predicate.Op}
	switch predicate.Op {
	case OpBetween:
		if predicate.Lo == nil || predicate.Hi == nil {
			return CostPredicate{}, Validationf("between predicate on %q requires lo and hi", predicate.Column)
		}
		out.Lo, out.Hi = predicate.Lo, predicate.Hi
	case OpEq, OpLt, OpGt, OpLte, OpGte:
		if predicate.Value == nil {
			return CostPredicate{}, Validationf("predicate on %q requires a value", predicate.Column)
		}
		out.Value = predicate.Value
	default:
		return CostPredicate{}, unsupportedf("predicate on %q uses unsupported operator %q", predicate.Column, predicate.Op)
	}
	return out, nil
}

// NullCostPredicate builds an IS NULL or IS NOT NULL estimator predicate. The
// query engine itself never pushes null tests down, so these only reach the
// estimator through direct callers.
func NullCostPredicate(column, typ string, isNull bool) CostPredicate {
	if isNull {
		return CostPredicate{Column: column, Type: typ, IsNull: true}
	}
	return CostPredicate{Column: column, Type: typ, IsNotNull: true}
}
