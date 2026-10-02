package columna

import (
	"bytes"
	"math"
	"math/bits"
)

// Page encodings supported inside a data page.
const (
	EncodingPlain      = "plain"
	EncodingDictionary = "dictionary"
	EncodingRLE        = "rle"
)

// PagePayload holds the encoded payload of one column page plus the
// encoding-specific header fields.
type PagePayload struct {
	Encoding  string
	DictSize  uint32
	RunCount  uint32
	IndexBits uint8
	Payload   []byte
}

// ---- primitive codecs -------------------------------------------------------

func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func readVarint(src []byte, off int) (uint64, int, error) {
	var value uint64
	var shift uint
	for {
		if off >= len(src) {
			return 0, 0, Formatf("truncated varint")
		}
		b := src[off]
		off++
		if shift > 63 {
			return 0, 0, Formatf("varint overflows 64 bits")
		}
		value |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return value, off, nil
		}
		shift += 7
	}
}

// packBits writes values LSB-first: the first value occupies the low bits of
// byte 0, the next value continues in the following bits. Every value occupies
// exactly width bits, and the final partial byte is zero padded.
func packBits(values []uint64, width int) []byte {
	if width == 0 {
		return nil
	}
	out := make([]byte, (len(values)*width+7)/8)
	bit := 0
	for _, v := range values {
		for i := 0; i < width; i++ {
			if v>>uint(i)&1 == 1 {
				out[bit/8] |= 1 << uint(bit%8)
			}
			bit++
		}
	}
	return out
}

func unpackBits(src []byte, width, count int) ([]uint64, error) {
	if width == 0 {
		return make([]uint64, count), nil
	}
	if len(src)*8 < count*width {
		return nil, Formatf("bit-packed payload is truncated")
	}
	out := make([]uint64, count)
	bit := 0
	for i := 0; i < count; i++ {
		var v uint64
		for b := 0; b < width; b++ {
			if src[bit/8]>>uint(bit%8)&1 == 1 {
				v |= 1 << uint(b)
			}
			bit++
		}
		out[i] = v
	}
	return out, nil
}

// PackNulls encodes the null map as 1 bit per value, LSB-first, where a set bit
// means NULL.
func PackNulls(values []Value) []byte {
	out := make([]byte, (len(values)+7)/8)
	for i, v := range values {
		if v.Null {
			out[i/8] |= 1 << uint(i%8)
		}
	}
	return out
}

func readNulls(src []byte, count int) ([]bool, error) {
	if len(src) < (count+7)/8 {
		return nil, Formatf("null bitmap is truncated")
	}
	nulls := make([]bool, count)
	for i := 0; i < count; i++ {
		nulls[i] = src[i/8]>>uint(i%8)&1 == 1
	}
	return nulls, nil
}

// encodePlainValues returns the concatenation of the plain payloads of the
// non-null values in order (used for the value body and for dictionaries).
func encodePlainValues(values []Value, indices []int) []byte {
	var out []byte
	for _, i := range indices {
		v := values[i]
		switch v.Kind {
		case TypeInt64:
			out = appendLE64(out, uint64(v.Int))
		case TypeFloat64:
			out = appendLE64(out, math.Float64bits(v.Float))
		case TypeBool:
			if v.Bool {
				out = append(out, 1)
			} else {
				out = append(out, 0)
			}
		default:
			out = append(out, v.Str...)
			out = append(out, 0)
		}
	}
	return out
}

func appendLE64(dst []byte, v uint64) []byte {
	return append(dst,
		byte(v), byte(v>>8), byte(v>>16), byte(v>>24),
		byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
}

func readLE64(src []byte, off int) (uint64, int, error) {
	if off+8 > len(src) {
		return 0, 0, Formatf("plain payload is truncated")
	}
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(src[off+i])
	}
	return v, off + 8, nil
}

// decodePlainValues reads count values of the given type from a plain payload.
func decodePlainValues(src []byte, typ string, count int) ([]Value, error) {
	out := make([]Value, count)
	off := 0
	for i := 0; i < count; i++ {
		switch typ {
		case TypeInt64:
			raw, next, err := readLE64(src, off)
			if err != nil {
				return nil, err
			}
			off = next
			out[i] = IntValue(int64(raw))
		case TypeFloat64:
			raw, next, err := readLE64(src, off)
			if err != nil {
				return nil, err
			}
			off = next
			out[i] = FloatValue(math.Float64frombits(raw))
		case TypeBool:
			if off >= len(src) {
				return nil, Formatf("plain payload is truncated")
			}
			out[i] = BoolValue(src[off] != 0)
			off++
		default:
			end := off
			for end < len(src) && src[end] != 0 {
				end++
			}
			if end >= len(src) {
				return nil, Formatf("string payload is missing its NUL terminator")
			}
			out[i] = StringValue(string(src[off:end]))
			off = end + 1
		}
	}
	return out, nil
}

func plainEncodedSize(values []Value, indices []int) int {
	size := 0
	for _, i := range indices {
		switch values[i].Kind {
		case TypeInt64, TypeFloat64:
			size += 8
		case TypeBool:
			size++
		default:
			size += len(values[i].Str) + 1
		}
	}
	return size
}

// ---- page encoders ----------------------------------------------------------

// PageStats describes one column page or one row group.
type PageStats struct {
	Nulls int64
	Set   bool
	Min   Value
	Max   Value
}

func (s PageStats) JSON() map[string]any {
	out := map[string]any{"null_count": s.Nulls}
	if s.Set {
		out["min"] = s.Min.JSON()
		out["max"] = s.Max.JSON()
	} else {
		out["min"] = nil
		out["max"] = nil
	}
	return out
}

func valuesStats(values []Value, indices []int) PageStats {
	// An all-NULL page has no boundary values; leaving Min and Max as NULL is
	// what makes the footer record them as absent instead of as an untyped
	// value that no reader could decode.
	stats := PageStats{Nulls: int64(len(values) - len(indices)), Min: NullValue(), Max: NullValue()}
	for _, i := range indices {
		v := values[i]
		if !stats.Set {
			stats.Set, stats.Min, stats.Max = true, v, v
			continue
		}
		if less(v, stats.Min) {
			stats.Min = v
		}
		if less(stats.Max, v) {
			stats.Max = v
		}
	}
	return stats
}

// less orders two non-null values of the same logical type.
func less(a, b Value) bool {
	switch a.Kind {
	case TypeInt64:
		return a.Int < b.Int
	case TypeFloat64:
		return a.Float < b.Float
	case TypeBool:
		return !a.Bool && b.Bool
	default:
		return a.Str < b.Str
	}
}

// encodePage builds the full page payload: the null bitmap followed by the
// encoding specific body. The layout is documented in README.md.
func encodePage(values []Value, typ string, requested string) (PagePayload, PageStats, error) {
	nonNull := make([]int, 0, len(values))
	for i := range values {
		if !values[i].Null {
			nonNull = append(nonNull, i)
		}
	}
	for _, i := range nonNull {
		if values[i].Kind != typ {
			return PagePayload{}, PageStats{}, Validationf("value kind %q does not match column type %q", values[i].Kind, typ)
		}
	}
	stats := valuesStats(values, nonNull)
	bitmap := PackNulls(values)
	var payload PagePayload
	switch requested {
	case EncodingPlain:
		payload = PagePayload{Encoding: EncodingPlain, Payload: encodePlainValues(values, nonNull)}
	case EncodingDictionary:
		encoded, err := dictionaryBody(values, nonNull)
		if err != nil {
			return PagePayload{}, PageStats{}, err
		}
		payload = encoded
	case EncodingRLE:
		encoded, err := rleBody(values, nonNull)
		if err != nil {
			return PagePayload{}, PageStats{}, err
		}
		payload = encoded
	default:
		return PagePayload{}, PageStats{}, Validationf("unsupported encoding %q", requested)
	}
	full := make([]byte, 0, len(bitmap)+len(payload.Payload))
	full = append(full, bitmap...)
	full = append(full, payload.Payload...)
	payload.Payload = full
	return payload, stats, nil
}

func dictionaryBody(values []Value, nonNull []int) (PagePayload, error) {
	if len(nonNull) == 0 {
		return PagePayload{Encoding: EncodingDictionary, IndexBits: 0}, nil
	}
	seen := map[string]int{}
	var dict []int
	var idxValues []uint64
	for _, i := range nonNull {
		key := valueKey(values[i])
		pos, ok := seen[key]
		if !ok {
			pos = len(dict)
			seen[key] = pos
			dict = append(dict, i)
		}
		idxValues = append(idxValues, uint64(pos))
	}
	width := 0
	if len(dict) > 1 {
		width = bits.Len(uint(len(dict) - 1))
	}
	return PagePayload{
		Encoding:  EncodingDictionary,
		DictSize:  uint32(len(dict)),
		IndexBits: uint8(width),
		Payload:   append(encodePlainValues(values, dict), packBits(idxValues, width)...),
	}, nil
}

func valueKey(v Value) string {
	switch v.Kind {
	case TypeInt64:
		return "i" + string(appendLE64(nil, uint64(v.Int)))
	case TypeFloat64:
		return "f" + string(appendLE64(nil, math.Float64bits(v.Float)))
	case TypeBool:
		if v.Bool {
			return "b1"
		}
		return "b0"
	default:
		return "s" + v.Str
	}
}

func rleBody(values []Value, nonNull []int) (PagePayload, error) {
	if len(nonNull) == 0 {
		return PagePayload{Encoding: EncodingRLE, RunCount: 0}, nil
	}
	for i := 1; i < len(nonNull); i++ {
		if nonNull[i] != nonNull[i-1]+1 {
			return PagePayload{}, Validationf("rle encoding requires physically contiguous non-null values")
		}
	}
	type run struct {
		length int
		sample int
	}
	var runs []run
	for _, i := range nonNull {
		if len(runs) > 0 && valueKey(values[runs[len(runs)-1].sample]) == valueKey(values[i]) {
			runs[len(runs)-1].length++
			continue
		}
		runs = append(runs, run{length: 1, sample: i})
	}
	body := bytes.Buffer{}
	for _, r := range runs {
		body.Write(appendVarint(nil, uint64(r.length)))
	}
	for _, r := range runs {
		body.Write(encodePlainValues(values, []int{r.sample}))
	}
	return PagePayload{Encoding: EncodingRLE, RunCount: uint32(len(runs)), Payload: body.Bytes()}, nil
}

// ---- page decoders ----------------------------------------------------------

func decodePage(header PageHeader, payload []byte, valueCount int, typ string) ([]Value, error) {
	nullBytes := (valueCount + 7) / 8
	if len(payload) < nullBytes {
		return nil, Formatf("page %d payload is truncated", header.PageIndex)
	}
	nulls, err := readNulls(payload[:nullBytes], valueCount)
	if err != nil {
		return nil, err
	}
	body := payload[nullBytes:]
	nonNull := 0
	for _, isNull := range nulls {
		if !isNull {
			nonNull++
		}
	}
	var values []Value
	switch header.Encoding {
	case EncodingPlain:
		values, err = decodePlainValues(body, typ, nonNull)
	case EncodingDictionary:
		values, err = decodeDictionary(header, body, nonNull, typ)
	case EncodingRLE:
		values, err = decodeRLE(header, body, nonNull)
	default:
		return nil, Formatf("unsupported encoding %q in page %d", header.Encoding, header.PageIndex)
	}
	if err != nil {
		return nil, err
	}
	out := make([]Value, valueCount)
	next := 0
	for i := 0; i < valueCount; i++ {
		if nulls[i] {
			out[i] = NullValue()
			continue
		}
		out[i] = values[next]
		next++
	}
	return out, nil
}

func decodeDictionary(header PageHeader, body []byte, nonNull int, typ string) ([]Value, error) {
	if header.DictSize == 0 {
		if nonNull != 0 {
			return nil, Formatf("page %d has dictionary size 0 but %d non-null values", header.PageIndex, nonNull)
		}
		return nil, nil
	}
	dict, err := decodePlainValues(body, typ, int(header.DictSize))
	if err != nil {
		return nil, err
	}
	consumed := plainEncodedSize(dict, indexRange(len(dict)))
	indices, err := unpackBits(body[consumed:], int(header.IndexBits), nonNull)
	if err != nil {
		return nil, err
	}
	out := make([]Value, nonNull)
	for i, idx := range indices {
		if idx >= uint64(len(dict)) {
			return nil, Formatf("page %d dictionary index %d is out of range", header.PageIndex, idx)
		}
		out[i] = dict[idx]
	}
	return out, nil
}

func indexRange(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func decodeRLE(header PageHeader, body []byte, nonNull int) ([]Value, error) {
	runs := int(header.RunCount)
	lengths := make([]int, runs)
	off := 0
	total := 0
	for i := 0; i < runs; i++ {
		value, next, err := readVarint(body, off)
		if err != nil {
			return nil, err
		}
		off = next
		if value == 0 {
			return nil, Formatf("page %d contains a zero-length run", header.PageIndex)
		}
		lengths[i] = int(value)
		total += int(value)
	}
	if total != nonNull {
		return nil, Formatf("page %d run lengths sum to %d but the page holds %d non-null values", header.PageIndex, total, nonNull)
	}
	// Run values follow the run-length block; the column type comes from the
	// page header so every run value uses the same plain layout.
	return decodeRunValues(body[off:], header, lengths)
}

func decodeRunValues(body []byte, header PageHeader, lengths []int) ([]Value, error) {
	typ := header.ColumnType
	out := make([]Value, 0, len(lengths))
	off := 0
	for _, length := range lengths {
		single, err := decodePlainValues(body[off:], typ, 1)
		if err != nil {
			return nil, err
		}
		size := plainEncodedSize(single, []int{0})
		off += size
		for i := 0; i < length; i++ {
			out = append(out, single[0])
		}
	}
	return out, nil
}

// SortedForRLE reports whether the non-null prefix of values is strictly
// ascending, which is the precondition for run-length encoding to be lossless
// and useful.
func SortedForRLE(values []Value) bool {
	previous := Value{}
	seen := false
	for _, v := range values {
		if v.Null {
			continue
		}
		if seen && less(v, previous) {
			return false
		}
		previous, seen = v, true
	}
	return true
}

func DistinctCount(values []Value) int {
	seen := map[string]bool{}
	for _, v := range values {
		if v.Null {
			continue
		}
		seen[valueKey(v)] = true
	}
	return len(seen)
}
