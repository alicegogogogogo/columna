package columna

import (
	"encoding/json"
	"math"
	"strings"
)

// Predicate operators accepted by POST /query.
const (
	OpEq      = "="
	OpLt      = "<"
	OpGt      = ">"
	OpLte     = "<="
	OpGte     = ">="
	OpBetween = "between"
)

// Aggregation functions accepted by POST /query.
const (
	AggCount = "count"
	AggSum   = "sum"
	AggMin   = "min"
	AggMax   = "max"
)

// Predicate is one pushed-down filter condition.
type Predicate struct {
	Column string   `json:"column"`
	Op     string   `json:"op"`
	Value  *Value   `json:"-"`
	Lo     *Value   `json:"-"`
	Hi     *Value   `json:"-"`
	Raw    []string `json:"-"`
}

// Query is a projection / filter / aggregation request.
//
// Pages (the JSON field max_pages_per_group) bounds the scan: only the first
// Pages pages of each column in each row group are considered. Pages beyond the
// bound are ignored by pushdown and by a full scan alike, so both modes still
// return the same rows.
type Query struct {
	Table      string      `json:"table"`
	Projection []string    `json:"projection"`
	Predicates []Predicate `json:"predicates"`
	Aggregates []Aggregate `json:"aggregates"`
	Limit      *int        `json:"limit"`
	Pushdown   *bool       `json:"pushdown"`
	Pages      *int        `json:"max_pages_per_group"`
}

// Aggregate is one aggregation expression.
type Aggregate struct {
	Func   string `json:"func"`
	Column string `json:"column"`
}

// QueryResult is the response body of POST /query.
type QueryResult struct {
	Columns       []string         `json:"columns"`
	Rows          []map[string]any `json:"rows"`
	RowCount      int              `json:"row_count"`
	ScannedRows   int              `json:"scanned_rows"`
	ScannedGroups int              `json:"scanned_row_groups"`
	ScannedPages  int              `json:"scanned_pages"`
	SkippedPages  int              `json:"skipped_pages"`
	Pushdown      bool             `json:"pushdown"`
	MatchedRows   int              `json:"matched_rows"`
}

var validOps = map[string]bool{OpEq: true, OpLt: true, OpGt: true, OpLte: true, OpGte: true, OpBetween: true}
var validAggs = map[string]bool{AggCount: true, AggSum: true, AggMin: true, AggMax: true}

func decodePredicate(raw map[string]json.RawMessage) (Predicate, error) {
	allowed := map[string]bool{"column": true, "op": true, "value": true, "lo": true, "hi": true}
	for key := range raw {
		if !allowed[key] {
			return Predicate{}, Validationf("unknown predicate field %q", key)
		}
	}
	predicate := Predicate{}
	columnRaw, ok := raw["column"]
	if !ok {
		return Predicate{}, Validationf("predicate is missing column")
	}
	if err := json.Unmarshal(columnRaw, &predicate.Column); err != nil {
		return Predicate{}, Validationf("predicate column must be a string")
	}
	opRaw, ok := raw["op"]
	if !ok {
		return Predicate{}, Validationf("predicate is missing op")
	}
	if err := json.Unmarshal(opRaw, &predicate.Op); err != nil {
		return Predicate{}, Validationf("predicate op must be a string")
	}
	if !validOps[predicate.Op] {
		return Predicate{}, Validationf("unsupported predicate op %q", predicate.Op)
	}
	switch predicate.Op {
	case OpBetween:
		if _, ok := raw["value"]; ok {
			return Predicate{}, Validationf("between takes lo and hi, not value")
		}
		lo, err := decodePredicateValue(raw["lo"], "lo", predicate.Op)
		if err != nil {
			return Predicate{}, err
		}
		hi, err := decodePredicateValue(raw["hi"], "hi", predicate.Op)
		if err != nil {
			return Predicate{}, err
		}
		predicate.Lo, predicate.Hi = lo, hi
	default:
		if _, ok := raw["lo"]; ok {
			return Predicate{}, Validationf("op %q takes value, not lo", predicate.Op)
		}
		if _, ok := raw["hi"]; ok {
			return Predicate{}, Validationf("op %q takes value, not hi", predicate.Op)
		}
		value, err := decodePredicateValue(raw["value"], "value", predicate.Op)
		if err != nil {
			return Predicate{}, err
		}
		predicate.Value = value
	}
	return predicate, nil
}

func decodePredicateValue(raw json.RawMessage, field, op string) (*Value, error) {
	if len(raw) == 0 {
		return nil, Validationf("predicate op %q requires %s", op, field)
	}
	if string(raw) == "null" {
		return nil, Validationf("predicate %s must not be null", field)
	}
	value := Value{}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, Validationf("predicate %s must be a number, bool or string", field)
	}
	return &value, nil
}

func (p Predicate) bind(schema Schema) (Predicate, error) {
	if !validOps[p.Op] {
		return Predicate{}, Validationf("unsupported predicate op %q", p.Op)
	}
	column, ok := schema.Column(p.Column)
	if !ok {
		return Predicate{}, Validationf("unknown column %q", p.Column)
	}
	bind := func(v *Value, field string) error {
		if v == nil {
			return Validationf("predicate on %q requires %s", p.Column, field)
		}
		coerceNumber(v, column.Type)
		if v.Kind != column.Type {
			return Validationf("predicate %s %s expects a %s value, got %s", p.Column, field, column.Type, v.Kind)
		}
		if column.Type == TypeString && p.Op != OpEq && p.Op != OpBetween {
			return Validationf("string columns support only \"=\" and \"between\"")
		}
		return nil
	}
	if p.Op == OpBetween {
		if err := bind(p.Lo, "lo"); err != nil {
			return Predicate{}, err
		}
		if err := bind(p.Hi, "hi"); err != nil {
			return Predicate{}, err
		}
		if compareValues(*p.Lo, *p.Hi) > 0 {
			return Predicate{}, Validationf("between lo must not exceed hi")
		}
		return p, nil
	}
	if err := bind(p.Value, "value"); err != nil {
		return Predicate{}, err
	}
	return p, nil
}

// coerceNumber lets one JSON number form serve both numeric column types: an
// int64 predicate value becomes a float64 for a float64 column, and an integral
// float64 value becomes an int64 for an int64 column. Anything else keeps its
// kind and is rejected by the caller.
func coerceNumber(v *Value, typ string) {
	const (
		minInt64AsFloat = -9223372036854775808.0
		maxInt64AsFloat = 9223372036854775808.0
	)
	switch {
	case typ == TypeFloat64 && v.Kind == TypeInt64:
		*v = FloatValue(float64(v.Int))
	case typ == TypeInt64 && v.Kind == TypeFloat64 &&
		v.Float == math.Trunc(v.Float) && v.Float >= minInt64AsFloat && v.Float < maxInt64AsFloat:
		*v = IntValue(int64(v.Float))
	}
}

// matches evaluates the predicate against one non-null value.
func (p Predicate) matches(v Value, typ string) bool {
	if v.Null {
		return false
	}
	switch p.Op {
	case OpEq:
		switch typ {
		case TypeString:
			return v.Str == p.Value.Str
		case TypeBool:
			return v.Bool == p.Value.Bool
		default:
			return compareValues(v, *p.Value) == 0
		}
	case OpBetween:
		if typ == TypeString {
			return v.Str >= p.Lo.Str && v.Str <= p.Hi.Str
		}
		return compareValues(v, *p.Lo) >= 0 && compareValues(v, *p.Hi) <= 0
	case OpLt, OpLte, OpGt, OpGte:
		if typ == TypeString {
			return false // ordering operators are rejected for string columns
		}
		cmp := compareValues(v, *p.Value)
		switch p.Op {
		case OpLt:
			return cmp < 0
		case OpLte:
			return cmp <= 0
		case OpGt:
			return cmp > 0
		case OpGte:
			return cmp >= 0
		}
	}
	return false
}

// compareValues orders two non-null values of the same logical type. int64 and
// float64 comparisons stay inside their own domain, so integers beyond 2^53
// keep full precision, and bools order false < true.
func compareValues(a, b Value) int {
	switch {
	case a.Kind == TypeInt64 && b.Kind == TypeInt64:
		switch {
		case a.Int < b.Int:
			return -1
		case a.Int > b.Int:
			return 1
		default:
			return 0
		}
	case a.Kind == TypeBool && b.Kind == TypeBool:
		switch {
		case a.Bool == b.Bool:
			return 0
		case !a.Bool:
			return -1
		default:
			return 1
		}
	case a.Kind == TypeString && b.Kind == TypeString:
		switch {
		case a.Str < b.Str:
			return -1
		case a.Str > b.Str:
			return 1
		default:
			return 0
		}
	default:
		x, _ := a.Number()
		y, _ := b.Number()
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		default:
			return 0
		}
	}
}

// pageMatches uses page statistics to decide whether a page can hold a
// matching row. It may only return false when no row in the page can match.
func (p Predicate) pageMatches(stats PageStats, rows int64) bool {
	if rows == 0 {
		return false
	}
	if stats.Nulls == rows {
		return false // every value is NULL, and NULL never matches
	}
	if !stats.Set {
		return true
	}
	// The statistics hold the min and max of the non-null values, so an
	// operator can only match if its value range overlaps [min, max].
	switch p.Op {
	case OpBetween:
		return compareValues(stats.Max, *p.Lo) >= 0 && compareValues(stats.Min, *p.Hi) <= 0
	case OpEq:
		return compareValues(stats.Min, *p.Value) <= 0 && compareValues(stats.Max, *p.Value) >= 0
	case OpLt:
		return compareValues(stats.Min, *p.Value) < 0
	case OpLte:
		return compareValues(stats.Min, *p.Value) <= 0
	case OpGt:
		return compareValues(stats.Max, *p.Value) > 0
	case OpGte:
		return compareValues(stats.Max, *p.Value) >= 0
	}
	return true
}

func (q Query) bind(table *Table) (Query, error) {
	if q.Table == "" {
		return q, Validationf("table is required")
	}
	if q.Table != table.Name {
		return q, Validationf("query targets table %q but table %q was loaded", q.Table, table.Name)
	}
	bound := q
	if len(bound.Projection) == 0 && len(bound.Aggregates) == 0 {
		for _, c := range table.Schema.Columns {
			bound.Projection = append(bound.Projection, c.Name)
		}
	}
	if len(bound.Aggregates) > 0 && len(q.Projection) > 0 {
		return bound, Validationf("projection and aggregates are mutually exclusive")
	}
	for _, name := range bound.Projection {
		if _, ok := table.Schema.Column(name); !ok {
			return bound, Validationf("unknown column %q in projection", name)
		}
	}
	for i := range bound.Predicates {
		bound.Predicates[i].Column = strings.TrimSpace(bound.Predicates[i].Column)
		predicate, err := bound.Predicates[i].bind(table.Schema)
		if err != nil {
			return bound, err
		}
		bound.Predicates[i] = predicate
	}
	for i, aggregate := range bound.Aggregates {
		if !validAggs[aggregate.Func] {
			return bound, Validationf("unsupported aggregate %q", aggregate.Func)
		}
		if aggregate.Func == AggCount && aggregate.Column == "" {
			continue
		}
		column, ok := table.Schema.Column(aggregate.Column)
		if !ok {
			return bound, Validationf("unknown column %q in aggregate %d", aggregate.Column, i)
		}
		if aggregate.Func == AggSum && column.Type != TypeInt64 && column.Type != TypeFloat64 {
			return bound, Validationf("sum requires an int64 or float64 column, got %s", column.Type)
		}
	}
	if bound.Limit != nil && *bound.Limit < 0 {
		return bound, Validationf("limit must not be negative")
	}
	if bound.Pages != nil && *bound.Pages < 0 {
		return bound, Validationf("max_pages_per_group must not be negative")
	}
	return bound, nil
}
