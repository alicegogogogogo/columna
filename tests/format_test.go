package tests

import (
	"os"
	"path/filepath"
	"testing"

	"columna/internal/columna"
)

func ints(values ...int64) []columna.Value {
	out := make([]columna.Value, len(values))
	for i, v := range values {
		out[i] = columna.IntValue(v)
	}
	return out
}

func TestVarintRoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 127, 128, 300, 1 << 20, 1<<64 - 1}
	for _, value := range cases {
		encoded := columna.AppendVarint(nil, value)
		decoded, next, err := columna.ReadVarint(encoded, 0)
		if err != nil {
			t.Fatalf("read %d: %v", value, err)
		}
		if decoded != value || next != len(encoded) {
			t.Fatalf("varint %d round-tripped to %d at %d", value, decoded, next)
		}
	}
	if encoded := columna.AppendVarint(nil, 300); len(encoded) != 2 {
		t.Fatalf("300 should need two bytes, got %v", encoded)
	}
	if _, _, err := columna.ReadVarint([]byte{0x80}, 0); err == nil {
		t.Fatal("truncated varint must be rejected")
	}
}

func TestBitPackingIsLSBFirst(t *testing.T) {
	packed := columna.PackBits([]uint64{0, 1, 2}, 2)
	if len(packed) != 1 || packed[0] != 0x24 {
		t.Fatalf("packing 0,1,2 at 2 bits gave %v, want [0x24]", packed)
	}
	values, err := columna.UnpackBits([]byte{0x24, 0x01}, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{0, 1, 2, 0}
	for i := range want {
		if values[i] != want[i] {
			t.Fatalf("unpacked %v, want %v", values, want)
		}
	}
	if packed := columna.PackBits([]uint64{7, 7}, 0); len(packed) != 0 {
		t.Fatalf("width 0 must not emit bytes, got %v", packed)
	}
	if _, err := columna.UnpackBits([]byte{0x01}, 4, 8); err == nil {
		t.Fatal("a short packed payload must be rejected")
	}
}

func TestNullBitmapLayout(t *testing.T) {
	values := []columna.Value{
		columna.IntValue(1), columna.NullValue(), columna.IntValue(3), columna.NullValue(),
		columna.IntValue(5), columna.IntValue(6), columna.IntValue(7), columna.IntValue(8),
		columna.NullValue(),
	}
	bitmap := columna.PackNulls(values)
	if len(bitmap) != 2 {
		t.Fatalf("9 values need two bitmap bytes, got %d", len(bitmap))
	}
	if bitmap[0] != 0x0a || bitmap[1] != 0x01 {
		t.Fatalf("null bitmap is %v, want [0x0a 0x01]", bitmap)
	}
	nulls, err := columna.ReadNulls(bitmap, len(values))
	if err != nil {
		t.Fatal(err)
	}
	for i, value := range values {
		if nulls[i] != value.Null {
			t.Fatalf("null flag %d is %v", i, nulls[i])
		}
	}
}

func TestEncodingRoundTripWithNulls(t *testing.T) {
	values := []columna.Value{
		columna.IntValue(7), columna.IntValue(7), columna.IntValue(7),
		columna.NullValue(), columna.IntValue(9),
	}
	for _, encoding := range []string{columna.EncodingPlain, columna.EncodingDictionary, columna.EncodingRLE} {
		if encoding == columna.EncodingRLE {
			continue // interior nulls are not run-length encodable
		}
		page, _, err := columna.EncodePage(values, columna.TypeInt64, encoding)
		if err != nil {
			t.Fatalf("%s: %v", encoding, err)
		}
		header := columna.PageHeaderFor(page, len(values), columna.TypeInt64)
		decoded, err := columna.DecodePage(header, page.Payload, len(values), columna.TypeInt64)
		if err != nil {
			t.Fatalf("%s: %v", encoding, err)
		}
		for i := range values {
			if decoded[i] != values[i] {
				t.Fatalf("%s value %d decoded to %+v, want %+v", encoding, i, decoded[i], values[i])
			}
		}
	}
}

func TestEveryColumnTypeRoundTrips(t *testing.T) {
	cases := []struct {
		typ    string
		values []columna.Value
	}{
		{columna.TypeInt64, ints(-1, 0, 1, 1<<62, -(1 << 62))},
		{columna.TypeFloat64, []columna.Value{columna.FloatValue(1.5), columna.FloatValue(-0.25), columna.FloatValue(0)}},
		{columna.TypeBool, []columna.Value{columna.BoolValue(true), columna.BoolValue(false), columna.BoolValue(true)}},
		{columna.TypeString, []columna.Value{columna.StringValue(""), columna.StringValue("alpha"), columna.StringValue("béta")}},
	}
	for _, testCase := range cases {
		for _, encoding := range []string{columna.EncodingPlain, columna.EncodingDictionary, columna.EncodingRLE} {
			if encoding == columna.EncodingRLE && !columna.SortedForRLETest(testCase.values) {
				continue
			}
			page, _, err := columna.EncodePage(testCase.values, testCase.typ, encoding)
			if err != nil {
				t.Fatalf("%s/%s: %v", testCase.typ, encoding, err)
			}
			header := columna.PageHeaderFor(page, len(testCase.values), testCase.typ)
			decoded, err := columna.DecodePage(header, page.Payload, len(testCase.values), testCase.typ)
			if err != nil {
				t.Fatalf("%s/%s: %v", testCase.typ, encoding, err)
			}
			for i := range testCase.values {
				if decoded[i] != testCase.values[i] {
					t.Fatalf("%s/%s value %d decoded to %+v, want %+v", testCase.typ, encoding, i, decoded[i], testCase.values[i])
				}
			}
		}
	}
}

func TestDictionaryPageLayout(t *testing.T) {
	values := []columna.Value{columna.IntValue(5), columna.IntValue(9), columna.IntValue(5)}
	page, _, err := columna.EncodePage(values, columna.TypeInt64, columna.EncodingDictionary)
	if err != nil {
		t.Fatal(err)
	}
	if page.DictSize != 2 || page.IndexBits != 1 {
		t.Fatalf("dictionary header is dict=%d bits=%d, want 2 and 1", page.DictSize, page.IndexBits)
	}
	// 1 bitmap byte + 2 int64 dictionary entries (16 bytes) + one packed index byte.
	if want := 1 + 16 + 1; len(page.Payload) != want {
		t.Fatalf("payload is %d bytes, want %d", len(page.Payload), want)
	}
}

func TestRLERunsAreCountedAndRejectedForInteriorNulls(t *testing.T) {
	values := []columna.Value{
		columna.StringValue("a"), columna.StringValue("a"), columna.StringValue("a"),
		columna.StringValue("b"), columna.StringValue("b"),
	}
	page, _, err := columna.EncodePage(values, columna.TypeString, columna.EncodingRLE)
	if err != nil {
		t.Fatal(err)
	}
	if page.RunCount != 2 {
		t.Fatalf("run count is %d, want 2", page.RunCount)
	}
	// One bitmap byte, two run lengths (one byte each) and two NUL terminated
	// strings.
	if want := 1 + 2 + 2 + 2; len(page.Payload) != want {
		t.Fatalf("rle payload is %d bytes, want %d", len(page.Payload), want)
	}
	gapped := []columna.Value{columna.IntValue(1), columna.NullValue(), columna.IntValue(1)}
	if _, _, err := columna.EncodePage(gapped, columna.TypeInt64, columna.EncodingRLE); err == nil {
		t.Fatal("rle must reject interior nulls")
	}
}

func TestAutoEncodingChoices(t *testing.T) {
	constant := ints(1, 1, 1, 1, 1, 1, 1, 1)
	if got := columna.ChooseEncoding(constant, columna.TypeInt64); got != columna.EncodingDictionary {
		t.Fatalf("a constant column chose %s, want dictionary (9 bytes) over rle (10 bytes)", got)
	}
	sorted := make([]columna.Value, 0, 20)
	for i := 0; i < 20; i++ {
		for repeat := 0; repeat < 6; repeat++ {
			sorted = append(sorted, columna.IntValue(int64(i)))
		}
	}
	if got := columna.ChooseEncoding(sorted, columna.TypeInt64); got != columna.EncodingRLE {
		t.Fatalf("a sorted run-heavy column chose %s, want rle", got)
	}
	lowCardinality := []columna.Value{
		columna.StringValue("north"), columna.StringValue("south"),
		columna.StringValue("north"), columna.StringValue("south"),
		columna.StringValue("north"), columna.StringValue("south"),
	}
	if got := columna.ChooseEncoding(lowCardinality, columna.TypeString); got != columna.EncodingDictionary {
		t.Fatalf("low cardinality column chose %s, want dictionary", got)
	}
	highCardinality := []columna.Value{
		columna.StringValue("alpha"), columna.StringValue("bravo"), columna.StringValue("charlie"),
		columna.StringValue("delta"), columna.StringValue("echo"), columna.StringValue("foxtrot"),
	}
	if got := columna.ChooseEncoding(highCardinality, columna.TypeString); got != columna.EncodingPlain {
		t.Fatalf("distinct strings chose %s, want plain", got)
	}
}

func TestFooterPageStatisticsAreTruthful(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "events.columna")
	schema := columna.Schema{Columns: []columna.Column{
		{Name: "id", Type: columna.TypeInt64},
		{Name: "score", Type: columna.TypeFloat64, Nullable: true},
	}}
	rows := make([]columna.Row, 0, 2500)
	for i := 0; i < 2500; i++ {
		score := columna.FloatValue(float64(i) / 3)
		if i%7 == 0 {
			score = columna.NullValue()
		}
		rows = append(rows, columna.Row{columna.IntValue(int64(i)), score})
	}
	file, err := columna.WriteFile(path, schema, [][]columna.Row{rows}, 512, nil)
	if err != nil {
		t.Fatal(err)
	}
	if file.Stats().RowCount != 2500 || file.Stats().RowGroupCount != 1 {
		t.Fatalf("stats are %+v", file.Stats())
	}
	for _, name := range []string{"id", "score"} {
		values, err := file.ColumnValues(name)
		if err != nil {
			t.Fatal(err)
		}
		pagePages := file.RowGroups()[0].Pages[name]
		offset := 0
		for _, header := range pagePages {
			window := values[offset : offset+int(header.ValueCount)]
			recorded := columna.StatsOfPage(header)
			computed := columna.ValuesStats(window)
			if recorded.Nulls != computed.Nulls || recorded.Set != computed.Set {
				t.Fatalf("%s page %d recorded %+v, computed %+v", name, header.PageIndex, recorded, computed)
			}
			if recorded.Set && (recorded.Min != computed.Min || recorded.Max != computed.Max) {
				t.Fatalf("%s page %d min/max %v/%v, computed %v/%v", name, header.PageIndex, recorded.Min, recorded.Max, computed.Min, computed.Max)
			}
			offset += int(header.ValueCount)
		}
		if offset != 2500 {
			t.Fatalf("%s decoded %d values", name, offset)
		}
	}
}

func TestFileHeaderAndChecksum(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "t.columna")
	schema := columna.Schema{Columns: []columna.Column{{Name: "id", Type: columna.TypeInt64}}}
	if _, err := columna.WriteFile(path, schema, [][]columna.Row{{{columna.IntValue(1)}}}, 128, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw[:8]) != string(columna.MagicBytes()) {
		t.Fatalf("magic is %q", raw[:8])
	}
	if raw[8] != 0 || raw[9] != columna.FormatVersion {
		t.Fatalf("version bytes are %v", raw[8:10])
	}
	checksum, err := columna.Checksum(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(checksum) != 64 {
		t.Fatalf("checksum is %q", checksum)
	}
	corrupted := append([]byte{}, raw...)
	corrupted[12] ^= 0xff
	bad := filepath.Join(directory, "bad.columna")
	if err := os.WriteFile(bad, corrupted, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := columna.ReadFile(bad); err == nil {
		t.Fatal("a corrupted file must fail its checksum")
	}
	if _, err := columna.ReadFile(filepath.Join(directory, "missing.columna")); err == nil {
		t.Fatal("a missing file must be reported")
	}
}

func TestAppendAcceptsOnlyTheSameSchema(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "t.columna")
	schema := columna.Schema{Columns: []columna.Column{{Name: "id", Type: columna.TypeInt64}}}
	if _, err := columna.WriteFile(path, schema, [][]columna.Row{{{columna.IntValue(1)}}}, 128, nil); err != nil {
		t.Fatal(err)
	}
	other := columna.Schema{Columns: []columna.Column{{Name: "id", Type: columna.TypeString}}}
	if _, err := columna.AppendRows(path, other, []columna.Row{{columna.StringValue("x")}}, 128, nil); err == nil {
		t.Fatal("appending with another schema must be rejected")
	}
	file, err := columna.AppendRows(path, schema, []columna.Row{{columna.IntValue(2)}}, 128, nil)
	if err != nil {
		t.Fatal(err)
	}
	if file.Stats().RowCount != 2 || file.Stats().RowGroupCount != 2 {
		t.Fatalf("after append the stats are %+v", file.Stats())
	}
}

func TestRowValidation(t *testing.T) {
	schema := columna.Schema{Columns: []columna.Column{
		{Name: "id", Type: columna.TypeInt64},
		{Name: "note", Type: columna.TypeString, Nullable: true},
	}}
	if _, err := columna.DecodeRows(schema, []byte(`[{"id":1,"note":null}]`)); err != nil {
		t.Fatalf("valid row rejected: %v", err)
	}
	cases := map[string]string{
		"unknown field":  `[{"id":1,"note":null,"extra":1}]`,
		"missing field":  `[{"id":1}]`,
		"null in strict": `[{"id":null,"note":"x"}]`,
		"bad type":       `[{"id":"one","note":null}]`,
		"not an array":   `{"id":1,"note":null}`,
	}
	for name, body := range cases {
		if _, err := columna.DecodeRows(schema, []byte(body)); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}
