package columna

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strings"
)

// Logical column types supported by the format. The physical layout of every
// type is documented in README.md ("Page payload layout").
const (
	TypeInt64   = "int64"
	TypeFloat64 = "float64"
	TypeBool    = "bool"
	TypeString  = "string"
)

// Column is one schema field. Nullable columns may contain null values; a
// non-nullable column rejects them at append time.
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
}

// Schema is the ordered list of columns of a table.
type Schema struct {
	Columns []Column `json:"columns"`
}

func (s Schema) Column(name string) (Column, bool) {
	for _, c := range s.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// columnIndex returns the position of a column inside a schema, or -1.
func columnIndex(schema Schema, name string) int {
	for i, c := range schema.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// nonNullIndices lists the positions of the non-null values.
func nonNullIndices(values []Value) []int {
	out := make([]int, 0, len(values))
	for i := range values {
		if !values[i].Null {
			out = append(out, i)
		}
	}
	return out
}

// ValidName reports whether a table or column name is accepted.
func ValidName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func validType(t string) bool {
	switch t {
	case TypeInt64, TypeFloat64, TypeBool, TypeString:
		return true
	}
	return false
}

func (s Schema) Validate() error {
	if len(s.Columns) == 0 {
		return Validationf("schema must define at least one column")
	}
	if len(s.Columns) > 512 {
		return Validationf("schema has %d columns, the limit is 512", len(s.Columns))
	}
	seen := map[string]bool{}
	for i, c := range s.Columns {
		if !ValidName(c.Name) {
			return Validationf("column %d has an invalid name %q", i, c.Name)
		}
		if seen[c.Name] {
			return Validationf("duplicate column %q", c.Name)
		}
		seen[c.Name] = true
		if !validType(c.Type) {
			return Validationf("column %q has unsupported type %q", c.Name, c.Type)
		}
	}
	return nil
}

func (s Schema) Equal(other Schema) bool {
	if len(s.Columns) != len(other.Columns) {
		return false
	}
	for i := range s.Columns {
		if s.Columns[i] != other.Columns[i] {
			return false
		}
	}
	return true
}

// Value is one cell. Exactly one of the typed fields is meaningful, selected
// by Kind; Null reports SQL NULL.
type Value struct {
	Null  bool
	Kind  string
	Int   int64
	Float float64
	Bool  bool
	Str   string
}

func NullValue() Value           { return Value{Null: true} }
func IntValue(v int64) Value     { return Value{Kind: TypeInt64, Int: v} }
func FloatValue(v float64) Value { return Value{Kind: TypeFloat64, Float: v} }
func BoolValue(v bool) Value     { return Value{Kind: TypeBool, Bool: v} }
func StringValue(v string) Value { return Value{Kind: TypeString, Str: v} }

// UnmarshalJSON decodes a JSON scalar into a typed value. The kind is inferred
// from the JSON token: booleans and strings keep their type, and numbers become
// int64 when they are written without a fraction or exponent and fit an int64,
// otherwise float64. Objects, arrays and non-finite numbers are rejected.
func (v *Value) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return Validationf("value must be a JSON scalar")
	}
	switch trimmed[0] {
	case 'n':
		if string(trimmed) != "null" {
			return Validationf("value must be a JSON scalar")
		}
		*v = NullValue()
		return nil
	case 't', 'f':
		var b bool
		if err := json.Unmarshal(trimmed, &b); err != nil {
			return Validationf("value must be a JSON scalar")
		}
		*v = BoolValue(b)
		return nil
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return Validationf("value must be a JSON string")
		}
		if containsNUL(s) {
			return Validationf("value must not contain NUL bytes")
		}
		*v = StringValue(s)
		return nil
	case '{', '[':
		return Validationf("value must be a JSON scalar")
	default:
		var number json.Number
		if err := json.Unmarshal(trimmed, &number); err != nil {
			return Validationf("value must be a number")
		}
		if !strings.ContainsAny(number.String(), ".eE") {
			if i, err := number.Int64(); err == nil {
				*v = IntValue(i)
				return nil
			}
		}
		f, err := number.Float64()
		if err != nil {
			return Validationf("value must be a number")
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return Validationf("value must be finite")
		}
		*v = FloatValue(f)
		return nil
	}
}

// Number returns the numeric view of an int64 or float64 value.
func (v Value) Number() (float64, bool) {
	switch v.Kind {
	case TypeInt64:
		return float64(v.Int), true
	case TypeFloat64:
		return v.Float, true
	}
	return 0, false
}

// JSON renders the value for API responses and CLI output.
func (v Value) JSON() any {
	if v.Null {
		return nil
	}
	switch v.Kind {
	case TypeInt64:
		return v.Int
	case TypeFloat64:
		return v.Float
	case TypeBool:
		return v.Bool
	default:
		return v.Str
	}
}

// Row is an ordered list of cells, one per schema column.
type Row []Value

func (r Row) JSON(schema Schema) map[string]any {
	out := make(map[string]any, len(r))
	for i, c := range schema.Columns {
		if i < len(r) {
			out[c.Name] = r[i].JSON()
		}
	}
	return out
}

// DecodeLines converts a JSONL file (one JSON object per line) or a JSON array
// into typed rows.
func DecodeLines(schema Schema, raw []byte) ([]Row, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, Validationf("input is empty")
	}
	if trimmed[0] == '[' {
		return DecodeRows(schema, json.RawMessage(trimmed))
	}
	rows := []Row{}
	for number, line := range bytes.Split(trimmed, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var item map[string]json.RawMessage
		if err := unmarshalStrict(json.RawMessage(line), &item); err != nil {
			return nil, Validationf("line %d is not a JSON object: %v", number+1, err)
		}
		row, err := DecodeRow(schema, item)
		if err != nil {
			return nil, Validationf("line %d: %s", number+1, err.Error())
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// DecodeRows converts a JSON message into typed rows for the schema. Unknown
// keys, missing keys, non-nullable nulls and type mismatches are rejected.
func DecodeRows(schema Schema, raw json.RawMessage) ([]Row, error) {
	var items []map[string]json.RawMessage
	if err := unmarshalStrict(raw, &items); err != nil {
		return nil, Validationf("rows must be a JSON array of objects: %v", err)
	}
	rows := make([]Row, 0, len(items))
	for i, item := range items {
		row, err := DecodeRow(schema, item)
		if err != nil {
			return nil, Validationf("row %d: %s", i, err.Error())
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// DecodeRow converts one decoded JSON object into a typed row.
func DecodeRow(schema Schema, item map[string]json.RawMessage) (Row, error) {
	known := map[string]Column{}
	for _, c := range schema.Columns {
		known[c.Name] = c
	}
	for key := range item {
		if _, ok := known[key]; !ok {
			return nil, Validationf("unknown field %q", key)
		}
	}
	row := make(Row, len(schema.Columns))
	for i, c := range schema.Columns {
		raw, ok := item[c.Name]
		if !ok {
			return nil, Validationf("missing field %q", c.Name)
		}
		value, err := decodeCell(c, raw)
		if err != nil {
			return nil, err
		}
		row[i] = value
	}
	return row, nil
}

func decodeCell(c Column, raw json.RawMessage) (Value, error) {
	if string(raw) == "null" {
		if !c.Nullable {
			return Value{}, Validationf("column %q is not nullable", c.Name)
		}
		return NullValue(), nil
	}
	switch c.Type {
	case TypeInt64:
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil {
			return Value{}, Validationf("column %q expects an int64", c.Name)
		}
		v, err := number.Int64()
		if err != nil {
			return Value{}, Validationf("column %q expects an int64, got %s", c.Name, number.String())
		}
		return IntValue(v), nil
	case TypeFloat64:
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil {
			return Value{}, Validationf("column %q expects a float64", c.Name)
		}
		v, err := number.Float64()
		if err != nil {
			return Value{}, Validationf("column %q expects a float64, got %s", c.Name, number.String())
		}
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return Value{}, Validationf("column %q rejects non-finite values", c.Name)
		}
		return FloatValue(v), nil
	case TypeBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return Value{}, Validationf("column %q expects a bool", c.Name)
		}
		return BoolValue(b), nil
	default:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return Value{}, Validationf("column %q expects a string", c.Name)
		}
		if containsNUL(s) {
			return Value{}, Validationf("column %q rejects embedded NUL bytes", c.Name)
		}
		return StringValue(s), nil
	}
}

func containsNUL(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return true
		}
	}
	return false
}

func unmarshalStrict(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errTrailing
	}
	return nil
}

var errTrailing = &Error{Code: CodeValidation, Message: "unexpected trailing data"}
