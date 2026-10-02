package columna

import (
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Statistics-driven scan cost estimation.
//
// EstimateScanCost ranks candidate files or shards from metadata alone: it
// never decodes a page payload and never writes or rewrites a file. The estimate
// first drops columns the query does not read, then prunes row groups and pages
// whose recorded statistics make a predicate hit impossible, and finally turns
// the surviving bytes, the values that must be decoded, the predicate
// selectivities and a limit early stop into one unitless cost. The weights are
// fixed, so equal input and equal metadata always give an equal estimate.

// Fixed cost-model weights. The cost of one candidate is
//
//	bytes/1000 + 0.01*decodedValues + 0.001*processedRows.
const (
	costBytesPerPoint   = 1000.0
	costPerDecodedValue = 0.01
	costPerProcessedRow = 0.001
)

// InvalidArgumentError reports a candidate that violates the estimator input
// contract: an empty candidate list, negative counts, more nulls than rows, a
// maximum below the minimum or more distinct values than non-null rows.
type InvalidArgumentError struct{ Message string }

func (e *InvalidArgumentError) Error() string { return e.Message }

func invalidf(format string, args ...any) error {
	return &InvalidArgumentError{Message: fmt.Sprintf(format, args...)}
}

// UnsupportedPredicateError reports a predicate the cost model cannot price.
// Predicates are validated for every candidate before any estimate is
// produced, so a caller never receives a partially estimated result.
type UnsupportedPredicateError struct{ Message string }

func (e *UnsupportedPredicateError) Error() string { return e.Message }

func unsupportedf(format string, args ...any) error {
	return &UnsupportedPredicateError{Message: fmt.Sprintf(format, args...)}
}

// CostPredicate is one filter condition to price. Type is the logical type of
// the column the literal is compared against (one of TypeInt64, TypeFloat64,
// TypeBool and TypeString). A condition is one of:
//
//   - IsNull or IsNotNull: Value, Lo and Hi are ignored;
//   - Op "=", "<", ">", "<=", ">=": Value holds the literal;
//   - Op "between": Lo and Hi hold the inclusive bounds.
type CostPredicate struct {
	Column    string
	Type      string
	Op        string
	Value     *Value
	Lo        *Value
	Hi        *Value
	IsNull    bool
	IsNotNull bool
}

// PageStat is the metadata of one page of one column. Min and Max describe the
// non-null values; Set false means the boundaries are unknown, for example an
// all-NULL page. Nulls must never exceed Rows and Distinct must never exceed
// Rows-Nulls.
type PageStat struct {
	Rows     int64
	Nulls    int64
	Distinct int64
	Set      bool
	Min      Value
	Max      Value
}

// GroupStat describes one row group. Pages holds one page entry per read
// column, keyed by column name. Pages of different columns share page indexes:
// page i of every column covers the same rows of the group.
type GroupStat struct {
	Rows  int64
	Pages map[string][]PageStat
}

// ColumnInput is one candidate column: its per-page on-disk byte counts in row
// group then page index order, followed by the candidate-wide summary.
type ColumnInput struct {
	// Bytes holds one byte count per page, ordered across row groups and then
	// by page index. It must align with the page tables of Groups.
	Bytes    []int64
	Nulls    int64
	Distinct int64
	Set      bool
	Min      Value
	Max      Value
}

// CandidateSource is one file or shard the estimator may scan.
type CandidateSource struct {
	// Name identifies the candidate in the report. It does not affect cost.
	Name string
	// Rows is the total row count of the candidate.
	Rows int64
	// Groups holds the row groups in scan order. An empty list describes a
	// candidate that only exposes page byte counts and candidate-wide
	// statistics.
	Groups []GroupStat
	// Columns holds per-page byte counts and the candidate-wide summary of
	// every column the estimator may need.
	Columns map[string]ColumnInput
}

// ScanRequest is the public estimation input.
type ScanRequest struct {
	// Sources are the candidate files or shards, in preference-neutral order.
	Sources []CandidateSource
	// ReadColumns lists every column the scan touches (projection, predicate
	// and sort columns). Columns outside this list cost nothing.
	ReadColumns []string
	// ProjectionColumns lists the returned columns, a subset of ReadColumns.
	// An empty list means every read column is projected.
	ProjectionColumns []string
	Predicates        []CostPredicate
	// Limit bounds the returned rows. A nil limit is unlimited; zero
	// short-circuits every candidate to zero cost.
	Limit *int64
	// SortRows is how many filtered rows a sort must consume before it can
	// return anything (zero when the query does not sort). It delays the limit
	// early stop.
	SortRows int64
}

// CandidateCost is the estimate of one candidate, in input order.
type CandidateCost struct {
	Source               string  `json:"source"`
	RowGroups            int     `json:"row_groups"`
	Pages                int64   `json:"pages"`
	Bytes                int64   `json:"bytes"`
	DecodedValues        int64   `json:"decoded_values"`
	FilteredRows         int64   `json:"filtered_rows"`
	ReturnedRows         int64   `json:"returned_rows"`
	ProcessedRows        int64   `json:"processed_rows"`
	Cost                 float64 `json:"cost"`
	IncompleteStatistics bool    `json:"incomplete_statistics"`
	Reason               string  `json:"reason"`
}

// ScanCostResult is the estimator output: one estimate per candidate in input
// order and the name of the cheapest candidate.
type ScanCostResult struct {
	Candidates []CandidateCost `json:"candidates"`
	// Selected is the cheapest candidate. It is empty when limit is zero (no
	// candidate is chosen).
	Selected string `json:"selected"`
}

// EstimateScanCost prices the candidate sources without reading their data.
//
// Estimates preserve input order. The cheapest candidate wins and ties go to
// the first input candidate. Limit zero returns zero-cost estimates and
// selects nobody.
func EstimateScanCost(request ScanRequest) (ScanCostResult, error) {
	if len(request.Sources) == 0 {
		return ScanCostResult{}, invalidf("at least one candidate source is required")
	}
	if request.SortRows < 0 {
		return ScanCostResult{}, invalidf("sort_rows must not be negative")
	}
	if request.Limit != nil && *request.Limit < 0 {
		return ScanCostResult{}, invalidf("limit must not be negative")
	}
	read, projection, err := request.columnSets()
	if err != nil {
		return ScanCostResult{}, err
	}
	if err := validatePredicates(request.Predicates, read); err != nil {
		return ScanCostResult{}, err
	}
	for i := range request.Sources {
		if err := validateCandidate(request.Sources[i], read, request.Predicates); err != nil {
			return ScanCostResult{}, err
		}
	}

	result := ScanCostResult{Candidates: make([]CandidateCost, len(request.Sources))}
	limit := int64(-1)
	if request.Limit != nil {
		limit = *request.Limit
	}
	for i := range request.Sources {
		result.Candidates[i] = estimateCandidate(request.Sources[i], read, projection, request.Predicates, limit, request.SortRows)
	}
	if limit == 0 {
		return result, nil
	}
	selected := 0
	for i := 1; i < len(result.Candidates); i++ {
		if result.Candidates[i].Cost < result.Candidates[selected].Cost {
			selected = i
		}
	}
	result.Selected = request.Sources[selected].Name
	return result, nil
}

func (r ScanRequest) columnSets() (map[string]bool, map[string]bool, error) {
	read := map[string]bool{}
	for _, name := range r.ReadColumns {
		if name == "" {
			return nil, nil, invalidf("read column names must not be empty")
		}
		if read[name] {
			return nil, nil, invalidf("read column %q is listed more than once", name)
		}
		read[name] = true
	}
	projection := map[string]bool{}
	for _, name := range r.ProjectionColumns {
		if !read[name] {
			return nil, nil, invalidf("projection column %q is not among the read columns", name)
		}
		if projection[name] {
			return nil, nil, invalidf("projection column %q is listed more than once", name)
		}
		projection[name] = true
	}
	if len(projection) == 0 {
		for name := range read {
			projection[name] = true
		}
	}
	return read, projection, nil
}

// validatePredicates rejects operators the model cannot price before any
// candidate is inspected, so failure never depends on candidate order.
func validatePredicates(predicates []CostPredicate, read map[string]bool) error {
	for i, p := range predicates {
		if p.Column == "" {
			return invalidf("predicate %d is missing a column", i)
		}
		if !read[p.Column] {
			return invalidf("predicate %d on %q is not among the read columns", i, p.Column)
		}
		if p.IsNull || p.IsNotNull {
			if p.IsNull && p.IsNotNull {
				return invalidf("predicate %d on %q is both IS NULL and IS NOT NULL", i, p.Column)
			}
			if p.Op != "" || p.Value != nil || p.Lo != nil || p.Hi != nil {
				return invalidf("predicate %d on %q mixes a null test with another operator", i, p.Column)
			}
			continue
		}
		if !validCostType(p.Type) {
			return unsupportedf("predicate %d on %q has unsupported column type %q", i, p.Column, p.Type)
		}
		switch p.Op {
		case OpEq, OpLt, OpGt, OpLte, OpGte:
			if p.Value == nil || p.Value.Null {
				return invalidf("predicate %d on %q requires a non-null value", i, p.Column)
			}
			if !valueMatchesType(*p.Value, p.Type) {
				return invalidf("predicate %d on %q compares %s with %s", i, p.Column, p.Type, p.Value.Kind)
			}
			if p.Type == TypeString && p.Op != OpEq {
				return unsupportedf("string column %q does not support ordering operator %q", p.Column, p.Op)
			}
		case OpBetween:
			if p.Lo == nil || p.Hi == nil || p.Lo.Null || p.Hi.Null {
				return invalidf("between predicate on %q requires non-null lo and hi", p.Column)
			}
			if !valueMatchesType(*p.Lo, p.Type) || !valueMatchesType(*p.Hi, p.Type) {
				return invalidf("between predicate on %q has bounds of the wrong type", p.Column)
			}
			if compareValues(*p.Lo, *p.Hi) > 0 {
				return invalidf("between predicate on %q has lo above hi", p.Column)
			}
		default:
			return unsupportedf("predicate %d on %q uses unsupported operator %q", i, p.Column, p.Op)
		}
	}
	return nil
}

func validCostType(typ string) bool {
	switch typ {
	case TypeInt64, TypeFloat64, TypeBool, TypeString:
		return true
	}
	return false
}

// valueMatchesType reports whether a predicate literal is comparable against a
// column type. Numeric columns share the numeric domain, so an integer literal
// may price a float64 column and vice versa, mirroring query binding.
func valueMatchesType(v Value, typ string) bool {
	if v.Null {
		return false
	}
	switch typ {
	case TypeInt64, TypeFloat64:
		return v.Kind == TypeInt64 || v.Kind == TypeFloat64
	case TypeBool:
		return v.Kind == TypeBool
	case TypeString:
		return v.Kind == TypeString
	}
	return false
}

func validateBoundary(typ string, set bool, min, max Value) error {
	if !set {
		return nil
	}
	if min.Null || max.Null {
		return invalidf("a present min/max must not be null")
	}
	if min.Kind != max.Kind {
		return invalidf("min and max must have the same type")
	}
	if !validCostType(min.Kind) {
		return invalidf("min/max have unsupported type %q", min.Kind)
	}
	if compareValues(min, max) > 0 {
		return invalidf("min must not exceed max")
	}
	return nil
}

func validateCandidate(source CandidateSource, read map[string]bool, predicates []CostPredicate) error {
	label := source.Name
	if label == "" {
		label = "<unnamed>"
	}
	if source.Rows < 0 {
		return invalidf("candidate %q has a negative row count", label)
	}
	if source.Columns == nil {
		return invalidf("candidate %q is missing per-column metadata", label)
	}
	for column := range read {
		input, ok := source.Columns[column]
		if !ok {
			return invalidf("candidate %q is missing metadata for read column %q", label, column)
		}
		for i, n := range input.Bytes {
			if n < 0 {
				return invalidf("candidate %q column %q page %d has a negative byte count", label, column, i)
			}
		}
		if input.Nulls < 0 {
			return invalidf("candidate %q column %q has a negative null count", label, column)
		}
		if input.Distinct < 0 {
			return invalidf("candidate %q column %q has a negative distinct count", label, column)
		}
		if input.Nulls > source.Rows {
			return invalidf("candidate %q column %q reports %d nulls but only %d rows", label, column, input.Nulls, source.Rows)
		}
		if input.Distinct > source.Rows-input.Nulls {
			return invalidf("candidate %q column %q reports %d distinct values but only %d non-null rows", label, column, input.Distinct, source.Rows-input.Nulls)
		}
		if err := validateBoundary(column, input.Set, input.Min, input.Max); err != nil {
			return invalidf("candidate %q column %q: %v", label, column, err)
		}
	}

	if len(source.Groups) == 0 {
		// A boundary-free source exposes only page byte counts; the per-page
		// byte lists may have any non-negative length.
		return validateKinds(label, source, read, predicates)
	}
	readCols := sortedKeys(read)
	for column := range read {
		input := source.Columns[column]
		pageTotal := 0
		for _, group := range source.Groups {
			if group.Pages == nil {
				return invalidf("candidate %q row group is missing its page statistics", label)
			}
			pages, ok := group.Pages[column]
			if !ok {
				return invalidf("candidate %q row group is missing column %q", label, column)
			}
			pageTotal += len(pages)
		}
		if pageTotal != len(input.Bytes) {
			return invalidf("candidate %q column %q lists %d page byte counts but %d pages in its row groups", label, column, len(input.Bytes), pageTotal)
		}
	}
	for g, group := range source.Groups {
		if group.Rows < 0 {
			return invalidf("candidate %q row group %d has a negative row count", label, g)
		}
		pageCount := -1
		pageTables := make(map[string][]PageStat, len(readCols))
		for _, column := range readCols {
			pages := group.Pages[column]
			if pageCount < 0 {
				pageCount = len(pages)
			} else if len(pages) != pageCount {
				return invalidf("candidate %q row group %d columns disagree about the page count", label, g)
			}
			pageTables[column] = pages
		}
		if pageCount < 0 {
			pageCount = 0
		}
		// Page indexes are shared across columns, so page p covers the same
		// number of rows in every column.
		for p := 0; p < pageCount; p++ {
			var pageRows int64 = -1
			for _, column := range readCols {
				page := pageTables[column][p]
				if page.Rows < 0 || page.Nulls < 0 || page.Distinct < 0 {
					return invalidf("candidate %q row group %d column %q page %d has a negative count", label, g, column, p)
				}
				if pageRows < 0 {
					pageRows = page.Rows
				} else if page.Rows != pageRows {
					return invalidf("candidate %q row group %d page %d columns disagree about the row count", label, g, p)
				}
				if page.Nulls > page.Rows {
					return invalidf("candidate %q row group %d column %q page %d reports %d nulls but holds %d rows", label, g, column, p, page.Nulls, page.Rows)
				}
				if page.Distinct > page.Rows-page.Nulls {
					return invalidf("candidate %q row group %d column %q page %d reports %d distinct values but only %d non-null rows", label, g, column, p, page.Distinct, page.Rows-page.Nulls)
				}
				if err := validateBoundary(column, page.Set, page.Min, page.Max); err != nil {
					return invalidf("candidate %q row group %d column %q page %d: %v", label, g, column, p, err)
				}
			}
		}
		for _, column := range readCols {
			var covered int64
			for _, page := range pageTables[column] {
				covered += page.Rows
			}
			if covered != group.Rows {
				return invalidf("candidate %q row group %d column %q pages cover %d rows but the group holds %d", label, g, column, covered, group.Rows)
			}
		}
	}
	var groupRows int64
	for _, group := range source.Groups {
		groupRows += group.Rows
	}
	if groupRows != source.Rows {
		return invalidf("candidate %q row groups cover %d rows but the candidate declares %d", label, groupRows, source.Rows)
	}
	if err := validateKinds(label, source, read, predicates); err != nil {
		return err
	}
	return nil
}

// validateKinds ensures every recorded boundary of one column shares one type
// and that it agrees with the type predicates declare for that column. Numeric
// int64 and float64 statistics are interchangeable because numeric literals are
// compared in the numeric (float64) domain; bool and string statistics are not.
func validateKinds(label string, source CandidateSource, read map[string]bool, predicates []CostPredicate) error {
	domain := func(kind string) string {
		switch kind {
		case TypeInt64, TypeFloat64:
			return "numeric"
		case TypeBool:
			return "bool"
		case TypeString:
			return "string"
		}
		return kind
	}
	byColumn := map[string][]CostPredicate{}
	for _, p := range predicates {
		byColumn[p.Column] = append(byColumn[p.Column], p)
	}
	for column := range read {
		var kind string
		note := func(where, k string) error {
			if kind == "" {
				kind = k
				return nil
			}
			if k != kind {
				return invalidf("candidate %q column %q%s mixes %s and %s statistics", label, column, where, kind, k)
			}
			return nil
		}
		input := source.Columns[column]
		if input.Set {
			if err := note("", input.Min.Kind); err != nil {
				return err
			}
		}
		for g, group := range source.Groups {
			if group.Pages == nil {
				continue
			}
			for p, page := range group.Pages[column] {
				if page.Set {
					if err := note(" page "+strconv.Itoa(p)+" of row group "+strconv.Itoa(g), page.Min.Kind); err != nil {
						return err
					}
				}
			}
		}
		if kind != "" {
			for _, p := range byColumn[column] {
				if domain(p.Type) != domain(kind) {
					return invalidf("candidate %q column %q holds %s statistics but a predicate compares it as %s", label, column, kind, p.Type)
				}
			}
		}
	}
	return nil
}

// activePage is one page index that survives predicate pruning, together with
// the per-column statistics and byte counts recorded for it.
type activePage struct {
	group int
	index int
	rows  int64
	bytes map[string]int64
	stats map[string]PageStat
}

// estimateCandidate prices one source.
func estimateCandidate(source CandidateSource, read, _ map[string]bool, predicates []CostPredicate, limit, sortRows int64) CandidateCost {
	estimate := CandidateCost{Source: source.Name}
	if limit == 0 {
		estimate.Reason = "limit is zero: nothing is read"
		return estimate
	}
	readCols := sortedKeys(read)
	byColumn := map[string][]CostPredicate{}
	for _, p := range predicates {
		byColumn[p.Column] = append(byColumn[p.Column], p)
	}

	var active []activePage
	survivingGroups := map[int]bool{}
	if len(source.Groups) == 0 {
		// Boundary-free source: the candidate-wide summary can only prune the
		// whole candidate. One synthetic page stands in for every byte page.
		whole := true
		for column, columnPredicates := range byColumn {
			input := source.Columns[column]
			summary := PageStat{Rows: source.Rows, Nulls: input.Nulls, Distinct: input.Distinct, Set: input.Set, Min: input.Min, Max: input.Max}
			for _, p := range columnPredicates {
				if !pagePredicatePossible(summary, p) {
					whole = false
				}
			}
		}
		if !whole {
			estimate.Reason = "candidate-wide statistics pruned every row"
			return finish(estimate, nil, source, readCols, byColumn, limit, sortRows, true)
		}
		bytes := map[string]int64{}
		stats := map[string]PageStat{}
		var pages int64
		for _, column := range readCols {
			input := source.Columns[column]
			for _, n := range input.Bytes {
				bytes[column] += n
			}
			pages += int64(len(input.Bytes))
			stats[column] = PageStat{Rows: source.Rows, Nulls: input.Nulls, Distinct: input.Distinct, Set: input.Set, Min: input.Min, Max: input.Max}
		}
		if source.Rows == 0 {
			// An empty source carries no readable pages regardless of the byte
			// list length, matching a candidate whose row groups hold no rows.
			active = nil
		} else {
			active = []activePage{{group: -1, index: -1, rows: source.Rows, bytes: bytes, stats: stats}}
			estimate.Pages = pages
			survivingGroups[-1] = true
		}
	} else if len(readCols) == 0 {
		// A scan that reads no columns still processes every surviving row, but
		// it touches no pages or bytes.
		for g, group := range source.Groups {
			if group.Rows == 0 {
				continue
			}
			survivingGroups[g] = true
			active = append(active, activePage{group: g, index: -1, rows: group.Rows, bytes: map[string]int64{}, stats: map[string]PageStat{}})
		}
	} else {
		// cursor[column] is the running index into each column's flat per-page
		// byte list, which runs across row groups and then page indexes. It
		// advances for every page, pruned or not, so a surviving page is paired
		// with the byte count recorded for exactly that page.
		cursor := map[string]int{}
		for g, group := range source.Groups {
			if group.Rows == 0 {
				for _, column := range readCols {
					cursor[column] += len(group.Pages[column])
				}
				continue
			}
			pageCount := len(group.Pages[readCols[0]])
			pages := make([]activePage, 0, pageCount)
			for p := 0; p < pageCount; p++ {
				entry := activePage{group: g, index: p, bytes: map[string]int64{}, stats: map[string]PageStat{}}
				possible := true
				var rows int64 = -1
				for _, column := range readCols {
					page := group.Pages[column][p]
					byteCount := source.Columns[column].Bytes[cursor[column]]
					cursor[column]++
					for _, predicate := range byColumn[column] {
						if !pagePredicatePossible(page, predicate) {
							possible = false
						}
					}
					entry.stats[column] = page
					if possible {
						entry.bytes[column] = byteCount
					}
					if rows < 0 {
						rows = page.Rows
					}
				}
				if possible {
					entry.rows = rows
					pages = append(pages, entry)
				}
			}
			if len(pages) == 0 {
				estimate.Reason = appendClause(estimate.Reason, "row group "+strconv.Itoa(g)+" pruned by page statistics")
				continue
			}
			survivingGroups[g] = true
			active = append(active, pages...)
		}
	}

	if len(source.Groups) > 0 {
		estimate.RowGroups = len(survivingGroups)
	} else if source.Rows > 0 {
		estimate.RowGroups = 1
	}
	return finish(estimate, active, source, readCols, byColumn, limit, sortRows, len(source.Groups) == 0)
}

// finish combines selectivity and the limit early stop and fills the cost of
// one candidate.
func finish(estimate CandidateCost, active []activePage, source CandidateSource, readCols []string, byColumn map[string][]CostPredicate, limit, sortRows int64, boundaryFree bool) CandidateCost {
	incomplete := false
	for column, columnPredicates := range byColumn {
		if boundaryFree {
			input := source.Columns[column]
			if !statsSufficient(columnPredicates, input.Set, input.Distinct) {
				incomplete = true
			}
			continue
		}
		for _, entry := range active {
			page := entry.stats[column]
			if !statsSufficient(columnPredicates, page.Set, page.Distinct) {
				incomplete = true
			}
		}
	}

	var activeRows int64
	for _, entry := range active {
		activeRows += entry.rows
	}
	selectivity := combinedSelectivity(active, byColumn)
	filtered := float64(activeRows) * selectivity
	estimate.FilteredRows = clampRound(filtered)
	estimate.IncompleteStatistics = incomplete

	returned := estimate.FilteredRows
	if limit >= 0 && returned > limit {
		returned = limit
	}
	estimate.ReturnedRows = returned

	// The limit early stop: walk the surviving pages in scan order until enough
	// rows have been examined to collect the required filtered rows. Without a
	// sort the scan stops after `limit` matches; with a sort it must first feed
	// SortRows filtered rows to the sorter, then scan to the end of the page
	// that carries the last required row (page granularity). The ceil division
	// is exactly that page-granular walk.
	needRows := activeRows
	if limit >= 0 {
		target := limit
		if sortRows > 0 && sortRows > target {
			target = sortRows
		}
		if selectivity > 0 {
			need := int64(math.Ceil(float64(target) / selectivity))
			if need < needRows {
				needRows = need
			}
		}
	}

	var bytes, decoded, processed int64
	var pages int64
	readGroups := map[int]bool{}
	switch {
	case len(active) == 0:
		// Every row was pruned: nothing is read.
		estimate.RowGroups = 0
	case boundaryFree:
		// Physical page rows are unavailable for a boundary-free source, so its
		// bytes and values are counted in full and only the row-processing term
		// benefits from the early stop. Pages is the physical page count.
		entry := active[0]
		for _, column := range readCols {
			bytes += entry.bytes[column]
			decoded += entry.rows
		}
		pages = estimate.Pages
		processed = minInt64(activeRows, needRows)
	default:
		var walked int64
		for _, entry := range active {
			if walked >= needRows {
				break
			}
			readGroups[entry.group] = true
			for _, column := range readCols {
				bytes += entry.bytes[column]
			}
			pages += int64(len(readCols))
			decoded += entry.rows * int64(len(readCols))
			walked += entry.rows
		}
		processed = minInt64(walked, activeRows)
		estimate.RowGroups = len(readGroups)
	}
	estimate.Bytes = bytes
	estimate.DecodedValues = decoded
	estimate.ProcessedRows = processed
	estimate.Pages = pages
	estimate.Cost = float64(bytes)/costBytesPerPoint +
		costPerDecodedValue*float64(decoded) +
		costPerProcessedRow*float64(processed)

	estimate.Reason = buildReason(estimate, incomplete, limit, sortRows, estimate.Reason)
	return estimate
}

// statsSufficient reports whether one page or summary carries every statistic
// the predicates on its column actually consume. Null tests need none, range
// predicates need min/max and equality additionally needs a positive distinct
// count; missing statistics force selectivity one and an incomplete flag.
func statsSufficient(predicates []CostPredicate, boundaries bool, distinct int64) bool {
	for _, p := range predicates {
		if p.IsNull || p.IsNotNull {
			continue
		}
		if p.Op == OpEq {
			if !boundaries || distinct <= 0 {
				return false
			}
			continue
		}
		if !boundaries {
			return false
		}
	}
	return true
}

// combinedSelectivity multiplies the per-column selectivities; columns are
// assumed independent. Each column's selectivity is the row-weighted mean of
// its per-page selectivities over the surviving pages.
func combinedSelectivity(active []activePage, byColumn map[string][]CostPredicate) float64 {
	if len(byColumn) == 0 {
		return 1
	}
	combined := 1.0
	columns := make([]string, 0, len(byColumn))
	for column := range byColumn {
		columns = append(columns, column)
	}
	sort.Strings(columns)
	for _, column := range columns {
		var rows int64
		var weighted float64
		for _, entry := range active {
			page := entry.stats[column]
			s := 1.0
			for _, p := range byColumn[column] {
				s *= predicateSelectivity(page, p)
			}
			weighted += float64(entry.rows) * s
			rows += entry.rows
		}
		if rows > 0 {
			combined *= weighted / float64(rows)
		}
	}
	return combined
}

// pagePredicatePossible returns false only when no row of the page can satisfy
// the predicate. Absent boundaries never prune, and NULL never matches a
// comparison.
func pagePredicatePossible(page PageStat, p CostPredicate) bool {
	if p.IsNull {
		return page.Nulls > 0
	}
	if p.IsNotNull {
		return page.Nulls < page.Rows
	}
	if page.Rows == 0 || page.Nulls == page.Rows {
		return false
	}
	if !page.Set {
		return true
	}
	switch p.Op {
	case OpBetween:
		return compareValues(page.Max, *p.Lo) >= 0 && compareValues(page.Min, *p.Hi) <= 0
	case OpEq:
		return compareValues(page.Min, *p.Value) <= 0 && compareValues(page.Max, *p.Value) >= 0
	case OpLt:
		return compareValues(page.Min, *p.Value) < 0
	case OpLte:
		return compareValues(page.Min, *p.Value) <= 0
	case OpGt:
		return compareValues(page.Max, *p.Value) > 0
	case OpGte:
		return compareValues(page.Max, *p.Value) >= 0
	}
	return true
}

// predicateSelectivity implements the documented estimates:
//
//   - "=": non-null fraction divided by the number of distinct values;
//   - range comparisons and between: overlap of the predicate's comparable
//     interval with [min, max], times the non-null fraction (string ranges use
//     the non-null fraction once the intervals overlap);
//   - IS NULL / IS NOT NULL: the null / non-null fraction.
//
// A missing statistic yields one, never zero: a missing statistic must not
// turn into a zero cost.
func predicateSelectivity(page PageStat, p CostPredicate) float64 {
	nonNullFraction := 1.0
	if page.Rows > 0 {
		nonNullFraction = float64(page.Rows-page.Nulls) / float64(page.Rows)
	}
	if p.IsNull {
		if page.Rows == 0 {
			return 1
		}
		return float64(page.Nulls) / float64(page.Rows)
	}
	if p.IsNotNull {
		return nonNullFraction
	}
	if !page.Set {
		return 1
	}
	switch p.Op {
	case OpEq:
		if compareValues(page.Min, *p.Value) > 0 || compareValues(page.Max, *p.Value) < 0 {
			return 0
		}
		if page.Distinct <= 0 {
			return 1
		}
		return nonNullFraction / float64(page.Distinct)
	case OpBetween:
		return rangeSelectivity(page, p.Lo, p.Hi, nonNullFraction)
	case OpLt, OpLte, OpGt, OpGte:
		lo, hi := comparisonBounds(p)
		return rangeSelectivity(page, lo, hi, nonNullFraction)
	}
	return 1
}

// comparisonBounds turns a one-sided comparison into an interval over the
// value domain so one overlap routine prices every range predicate.
func comparisonBounds(p CostPredicate) (*Value, *Value) {
	switch p.Op {
	case OpLt, OpLte:
		return domainMin(p.Value.Kind), p.Value
	case OpGt, OpGte:
		return p.Value, domainMax(p.Value.Kind)
	}
	return p.Value, p.Value
}

func rangeSelectivity(page PageStat, lo, hi *Value, nonNullFraction float64) float64 {
	if lo == nil || hi == nil {
		return 1
	}
	if compareValues(page.Max, *lo) < 0 || compareValues(page.Min, *hi) > 0 {
		return 0
	}
	low, lowOK := lo.Number()
	high, highOK := hi.Number()
	min, minOK := page.Min.Number()
	max, maxOK := page.Max.Number()
	if !lowOK || !highOK || !minOK || !maxOK {
		// Non-numeric comparable domain (strings): the overlap fraction is not
		// measurable, so the non-null fraction is the conservative estimate.
		return nonNullFraction
	}
	if max <= min {
		return nonNullFraction
	}
	overlapMin := math.Max(low, min)
	overlapMax := math.Min(high, max)
	if overlapMax < overlapMin {
		return 0
	}
	return (overlapMax - overlapMin) / (max - min) * nonNullFraction
}

func domainMin(kind string) *Value {
	switch kind {
	case TypeInt64:
		v := IntValue(math.MinInt64)
		return &v
	case TypeFloat64:
		v := FloatValue(math.Inf(-1))
		return &v
	case TypeBool:
		v := BoolValue(false)
		return &v
	}
	return nil
}

func domainMax(kind string) *Value {
	switch kind {
	case TypeInt64:
		v := IntValue(math.MaxInt64)
		return &v
	case TypeFloat64:
		v := FloatValue(math.Inf(1))
		return &v
	case TypeBool:
		v := BoolValue(true)
		return &v
	}
	return nil
}

func clampRound(v float64) int64 {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	if v >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(math.Round(v))
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func buildReason(estimate CandidateCost, incomplete bool, limit, sortRows int64, pruning string) string {
	clauses := []string{}
	if estimate.RowGroups > 0 {
		clauses = append(clauses, strconv.Itoa(estimate.RowGroups)+" row groups survive pruning")
	} else if pruning == "" {
		clauses = append(clauses, "no rows survive pruning")
	}
	clauses = append(clauses,
		strconv.FormatInt(estimate.Pages, 10)+" pages",
		strconv.FormatInt(estimate.Bytes, 10)+" bytes",
		strconv.FormatInt(estimate.DecodedValues, 10)+" decoded values",
		strconv.FormatInt(estimate.ProcessedRows, 10)+" processed rows",
		strconv.FormatInt(estimate.FilteredRows, 10)+" rows after filtering",
	)
	if incomplete {
		clauses = append(clauses, "statistics incomplete: selectivity one where statistics are missing")
	}
	if limit >= 0 {
		clauses = append(clauses, "limit "+strconv.FormatInt(limit, 10)+" caps the returned rows")
	}
	if sortRows > 0 {
		clauses = append(clauses, "sort needs "+strconv.FormatInt(sortRows, 10)+" filtered rows before stopping")
	}
	if pruning != "" {
		clauses = append(clauses, pruning)
	}
	out := ""
	for i, clause := range clauses {
		if i > 0 {
			out += "; "
		}
		out += clause
	}
	return out
}

func appendClause(existing, clause string) string {
	if existing == "" {
		return clause
	}
	return existing + "; " + clause
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
