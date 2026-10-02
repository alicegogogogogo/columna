package columna

import (
	"encoding/json"
	"fmt"
	"sort"
)

// UnmarshalJSON makes predicate decoding field-aware: `value` is required for
// the comparison operators and `lo`/`hi` for `between`.
func (p *Predicate) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := unmarshalStrict(raw, &fields); err != nil {
		return Validationf("invalid predicate: %v", err)
	}
	decoded, err := decodePredicate(fields)
	if err != nil {
		return err
	}
	*p = decoded
	return nil
}

// DecodeQuery parses a POST /query body.
func DecodeQuery(raw json.RawMessage) (Query, error) {
	var query Query
	if err := unmarshalStrict(raw, &query); err != nil {
		return Query{}, Validationf("invalid query: %v", err)
	}
	return query, nil
}

// decodedSegment holds the values of one decoded page together with the
// absolute row offset the page starts at inside its row group.
type decodedSegment struct {
	start  int
	values []Value
}

// decodedColumn holds the decoded pages of one column. Pushdown may skip pages
// in the middle of a row group, so a column is a list of segments rather than
// one contiguous run of values.
type decodedColumn struct {
	// needed reports whether the query reads this column at all (projection,
	// predicate or aggregate). Rows are only required to be present for needed
	// columns.
	needed bool
	// segments holds the decoded pages in page index order.
	segments []decodedSegment
}

// valueAt returns the value of one row, or false when the row lies in a page
// that was not decoded.
func (c decodedColumn) valueAt(row int) (Value, bool) {
	index := sort.Search(len(c.segments), func(i int) bool { return c.segments[i].start > row })
	if index == 0 {
		return Value{}, false
	}
	segment := c.segments[index-1]
	offset := row - segment.start
	if offset < 0 || offset >= len(segment.values) {
		return Value{}, false
	}
	return segment.values[offset], true
}

type decodeStats struct {
	decodedPages int
	skippedPages int
}

// Execute binds and runs a query against one table.
//
// With pushdown enabled, every page whose recorded min/max statistics cannot
// satisfy a predicate is skipped before it is decoded, and a row group without
// surviving pages is skipped entirely. Predicate evaluation itself only looks
// at decoded values, so pushdown never changes the result set; it only changes
// how many pages and rows are touched.
func Execute(table *Table, query Query) (QueryResult, error) {
	bound, err := query.bind(table)
	if err != nil {
		return QueryResult{}, err
	}
	pushdown := true
	if bound.Pushdown != nil {
		pushdown = *bound.Pushdown
	}
	if len(bound.Aggregates) > 0 {
		return executeAggregates(table, bound, pushdown)
	}

	result := QueryResult{
		Columns:  append([]string{}, bound.Projection...),
		Rows:     []map[string]any{},
		Pushdown: pushdown,
	}
	limit := -1
	if bound.Limit != nil {
		limit = *bound.Limit
	}
	projection := columnIndices(table.Schema, bound.Projection)

	stop := false
	for groupIndex := range table.parsed.rowGroups {
		if stop {
			break
		}
		group := table.parsed.rowGroups[groupIndex]
		if group.RowCount == 0 {
			continue
		}
		result.ScannedGroups++
		columns, stats, err := decodeGroup(table, group, bound, pushdown)
		if err != nil {
			return QueryResult{}, err
		}
		result.ScannedPages += stats.decodedPages
		result.SkippedPages += stats.skippedPages

		examined, err := scanGroup(table.Schema, group.RowCount, columns, bound.Predicates, func(_ int, values []Value) bool {
			if limit >= 0 && len(result.Rows) >= limit {
				stop = true
				return false
			}
			row := make(map[string]any, len(projection))
			for _, index := range projection {
				row[table.Schema.Columns[index].Name] = values[index].JSON()
			}
			result.Rows = append(result.Rows, row)
			result.MatchedRows++
			return true
		})
		result.ScannedRows += examined
		if err != nil {
			return QueryResult{}, err
		}
	}
	result.RowCount = len(result.Rows)
	return result, nil
}

func columnIndices(schema Schema, names []string) []int {
	out := make([]int, 0, len(names))
	for _, name := range names {
		out = append(out, columnIndex(schema, name))
	}
	return out
}

// scanGroup walks the rows of one row group and calls visit for every row that
// satisfies all predicates, returning the number of rows it examined. A row is
// examined only when every column the query reads has a decoded value for it;
// rows that live in a pruned or budget-excluded page can never match and are
// not examined. Scanning stops as soon as visit returns false.
func scanGroup(schema Schema, rowCount int, columns []decodedColumn, predicates []Predicate, visit func(row int, values []Value) bool) (int, error) {
	type predicateColumn struct {
		index  int
		column Column
	}
	predicateColumns := make([]predicateColumn, 0, len(predicates))
	for _, predicate := range predicates {
		index := columnIndex(schema, predicate.Column)
		column, _ := schema.Column(predicate.Column)
		predicateColumns = append(predicateColumns, predicateColumn{index, column})
	}
	examined := 0
	values := make([]Value, len(schema.Columns))
	for row := 0; row < rowCount; row++ {
		available := true
		for c := range columns {
			if !columns[c].needed {
				continue
			}
			value, ok := columns[c].valueAt(row)
			if !ok {
				available = false
				break
			}
			values[c] = value
		}
		if !available {
			continue
		}
		examined++
		matched := true
		for i, entry := range predicateColumns {
			if !predicates[i].matches(values[entry.index], entry.column.Type) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		if !visit(row, values) {
			return examined, nil
		}
	}
	return examined, nil
}

// decodeGroup decodes every page that survives pushdown and the page budget,
// for the columns the query needs.
func decodeGroup(table *Table, group RowGroup, query Query, pushdown bool) ([]decodedColumn, decodeStats, error) {
	columns := make([]decodedColumn, len(table.Schema.Columns))
	stats := decodeStats{}
	if len(group.Pages) == 0 {
		return columns, stats, nil
	}
	width := pageWidth(group)
	budget := -1
	if query.Pages != nil {
		budget = *query.Pages
	}
	for index, column := range table.Schema.Columns {
		predicates := predicatesFor(query, column.Name)
		needed := len(predicates) > 0 || columnNeeded(query, column.Name)
		columns[index].needed = needed
		if !needed {
			continue
		}
		for _, header := range group.Pages[column.Name] {
			// The budget counts pages by their index, so a pruned page still
			// consumes its slot and both scan modes cover the same rows.
			if budget >= 0 && int(header.PageIndex) >= budget {
				break
			}
			if pushdown && !pagePossible(header, predicates) {
				stats.skippedPages++
				continue
			}
			values, err := table.parsed.pageValues(header)
			if err != nil {
				return nil, stats, err
			}
			if len(values) != int(header.ValueCount) {
				return nil, stats, Formatf("page %d decoded %d values but declares %d", header.PageIndex, len(values), header.ValueCount)
			}
			columns[index].segments = append(columns[index].segments, decodedSegment{
				start:  int(header.PageIndex) * width,
				values: values,
			})
			stats.decodedPages++
		}
	}
	return columns, stats, nil
}

// pageWidth is the page capacity recorded for a row group. Reading it back
// instead of guessing keeps page index to row offset mapping exact.
func pageWidth(group RowGroup) int { return normalizedPageWidth(group.PageValues) }

func predicatesFor(query Query, column string) []Predicate {
	var out []Predicate
	for _, predicate := range query.Predicates {
		if predicate.Column == column {
			out = append(out, predicate)
		}
	}
	return out
}

func projectionNeeds(query Query, column string) bool {
	for _, name := range query.Projection {
		if name == column {
			return true
		}
	}
	return false
}

// columnNeeded reports whether a query reads a column through its projection or
// through an aggregate.
func columnNeeded(query Query, column string) bool {
	if projectionNeeds(query, column) {
		return true
	}
	for _, aggregate := range query.Aggregates {
		if aggregate.Column == column {
			return true
		}
	}
	return false
}

// pagePossible applies the page statistics recorded in the footer. It returns
// false only when no row in the page can satisfy every predicate on the column.
func pagePossible(header PageHeader, predicates []Predicate) bool {
	if len(predicates) == 0 {
		return true
	}
	if header.ValueCount == 0 {
		return false
	}
	stats := header.stats
	for _, predicate := range predicates {
		if !predicate.pageMatches(stats, int64(header.ValueCount)) {
			return false
		}
	}
	return true
}

func executeAggregates(table *Table, query Query, pushdown bool) (QueryResult, error) {
	result := QueryResult{Columns: []string{}, Rows: []map[string]any{}, Pushdown: pushdown}
	state := make([]aggregateState, len(query.Aggregates))
	// Result columns are named <func>_<column>, or <func> for count over every
	// row. A repeated name gets a numeric suffix so the row object stays
	// unambiguous.
	used := map[string]int{}
	for i, aggregate := range query.Aggregates {
		name := aggregate.Func
		if aggregate.Column != "" {
			name = aggregate.Func + "_" + aggregate.Column
		}
		used[name]++
		if used[name] > 1 {
			name = fmt.Sprintf("%s_%d", name, used[name])
		}
		result.Columns = append(result.Columns, name)
		state[i] = aggregateState{
			function: aggregate.Func,
			column:   aggregate.Column,
			index:    columnIndex(table.Schema, aggregate.Column),
			min:      NullValue(),
			max:      NullValue(),
		}
	}
	for groupIndex := range table.parsed.rowGroups {
		group := table.parsed.rowGroups[groupIndex]
		if group.RowCount == 0 {
			continue
		}
		result.ScannedGroups++
		columns, stats, err := decodeGroup(table, group, query, pushdown)
		if err != nil {
			return QueryResult{}, err
		}
		result.ScannedPages += stats.decodedPages
		result.SkippedPages += stats.skippedPages
		examined, err := scanGroup(table.Schema, group.RowCount, columns, query.Predicates, func(_ int, values []Value) bool {
			result.MatchedRows++
			for i := range state {
				state[i].add(values)
			}
			return true
		})
		result.ScannedRows += examined
		if err != nil {
			return QueryResult{}, err
		}
	}
	row := map[string]any{}
	for i := range state {
		row[result.Columns[i]] = state[i].value()
	}
	result.Rows = append(result.Rows, row)
	result.RowCount = 1
	return result, nil
}

type aggregateState struct {
	function string
	column   string
	index    int
	count    int64
	sum      float64
	min      Value
	max      Value
}

func (a *aggregateState) add(values []Value) {
	if a.function == AggCount && a.column == "" {
		a.count++
		return
	}
	if a.index < 0 || a.index >= len(values) {
		return
	}
	value := values[a.index]
	if a.function == AggCount {
		if !value.Null {
			a.count++
		}
		return
	}
	if value.Null {
		return
	}
	switch a.function {
	case AggSum:
		number, ok := value.Number()
		if !ok {
			return
		}
		a.sum += number
	case AggMin:
		if a.min.Null || less(value, a.min) {
			a.min = value
		}
	case AggMax:
		if a.max.Null || less(a.max, value) {
			a.max = value
		}
	}
}

func (a *aggregateState) value() any {
	switch a.function {
	case AggCount:
		return a.count
	case AggSum:
		return a.sum
	case AggMin:
		if a.min.Null {
			return nil
		}
		return a.min.JSON()
	default:
		if a.max.Null {
			return nil
		}
		return a.max.JSON()
	}
}
