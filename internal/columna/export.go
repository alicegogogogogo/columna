package columna

// This file exposes the small pieces of the encoding layer that the external
// test package exercises directly. They are thin wrappers, so the tests cover
// exactly the code the file format uses.

// AppendVarint appends a LEB128 unsigned varint.
func AppendVarint(dst []byte, v uint64) []byte { return appendVarint(dst, v) }

// ReadVarint reads a LEB128 unsigned varint at offset off.
func ReadVarint(src []byte, off int) (uint64, int, error) { return readVarint(src, off) }

// PackBits writes values LSB-first using exactly width bits per value.
func PackBits(values []uint64, width int) []byte { return packBits(values, width) }

// UnpackBits reverses PackBits.
func UnpackBits(src []byte, width, count int) ([]uint64, error) { return unpackBits(src, width, count) }

// ReadNulls decodes a null bitmap produced by PackNulls.
func ReadNulls(src []byte, count int) ([]bool, error) { return readNulls(src, count) }

// Page describes an encoded page in exported form so the test package can
// check the byte layout without touching unexported fields.
type Page struct {
	Encoding  string
	DictSize  uint32
	RunCount  uint32
	IndexBits uint8
	Payload   []byte
}

// EncodePage encodes one page with an explicit encoding and returns its
// payload plus the statistics the footer will record for it.
func EncodePage(values []Value, typ, encoding string) (Page, PageStats, error) {
	payload, stats, err := encodePage(values, typ, encoding)
	if err != nil {
		return Page{}, PageStats{}, err
	}
	return Page{
		Encoding:  payload.Encoding,
		DictSize:  payload.DictSize,
		RunCount:  payload.RunCount,
		IndexBits: payload.IndexBits,
		Payload:   payload.Payload,
	}, stats, nil
}

// DecodePage decodes a page payload produced by EncodePage.
func DecodePage(header PageHeader, payload []byte, valueCount int, typ string) ([]Value, error) {
	return decodePage(header, payload, valueCount, typ)
}

// ChooseEncoding reports the encoding the writer would pick for a page.
func ChooseEncoding(values []Value, typ string) string { return chooseEncoding(values, typ) }

// ValuesStats recomputes the min/max/null summary of a page.
func ValuesStats(values []Value) PageStats { return valuesStats(values, nonNullIndices(values)) }

// StatsOfPage returns the statistics recorded in the footer for a page.
func StatsOfPage(header PageHeader) PageStats { return header.stats }

// SortedForRLETest reports whether values are ascending, which is the
// precondition for run-length encoding.
func SortedForRLETest(values []Value) bool { return SortedForRLE(values) }

// MagicBytes returns the 8 file magic bytes.
func MagicBytes() []byte { return append([]byte{}, Magic[:]...) }

// PageHeaderFor builds the header that matches a Page produced by EncodePage.
func PageHeaderFor(page Page, valueCount int, columnType string) PageHeader {
	return PageHeader{
		ValueCount: uint32(valueCount),
		Encoding:   page.Encoding,
		DictSize:   page.DictSize,
		RunCount:   page.RunCount,
		IndexBits:  page.IndexBits,
		ColumnType: columnType,
	}
}
