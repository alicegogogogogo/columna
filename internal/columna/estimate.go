package columna

import (
	"fmt"
	"math"
	"strings"
)

// Predicate operators the scan cost estimator understands in addition to the
// query engine's operators. The engine itself does not accept them; they
// exist so the estimator can price NULL tests.
const (
	OpIsNull    = "is_null"
	OpIsNotNull = "is_not_null"
)

// InvalidArgumentError reports a cost estimation request that violates the
// estimator's input contract: no candidate at all, a negative row, byte or
// null count, a null count above its page's row count, a maximum below its
// minimum or a distinct count above the non-null row count.
type InvalidArgumentError struct{ Message string }

func (e *InvalidArgumentError) Error() string { return e.Message }

func invalidArgumentf(format string, args ...any) *InvalidArgumentError {
	return &InvalidArgumentError{Message: fmt.Sprintf(format, args...)}
}

// UnsupportedPredicateError reports a predicate operator, or a predicate and
// column type combination, that the cost model cannot price. The estimator
// never returns a partial estimate for a predicate it does not understand.
type UnsupportedPredicateError struct{ Message string }

func (e *UnsupportedPredicateError) Error() string { return e.Message }

func unsupportedPredicatef(format string, args ...any) *UnsupportedPredicateError {
	return &UnsupportedPredicateError{Message: fmt.Sprintf(format, args...)}
}

// Cost weights of the unified scan cost: one point per kilobyte read, 0.01
// points per decoded value and 0.001 points per row that survives pruning.
const (
	costPerKilobyte = 1.0
	costPerValue    = 0.01
	costPerRow      = 0.001
)

// EstimateRequest is the input of EstimateScan: the candidate sources to
// price, the columns the scan would read, the projection, the predicates, the
// row limit and the number of rows an ordering operator must receive.
//
// ReadColumns lists the columns the scan would consider decoding; Projection
// lists the columns the caller actually returns. The estimator first drops
// every read column that neither the projection nor a predicate needs. Limit
// bounds the rows returned (nil means unlimited, zero means no scan at all)
// and SortRows is the row count a sort must receive before it can emit, so
// early stopping never cuts the scan below max(limit, sort_rows).
type EstimateRequest struct {
	Candidates  []SourceCandidate `json:"candidates"`
	ReadColumns []string          `json:"read_columns"`
	Projection  []string          `json:"projection"`
	Predicates  []Predicate       `json:"predicates"`
	Limit       *int64            `json:"limit"`
	SortRows    *int64            `json:"sort_rows"`
}

// SourceCandidate describes one scannable source (a file or a shard) through
// statistics only. Row groups may either enumerate their pages or carry
// column statistics directly, in which case the row group itself is the
// estimation unit.
type SourceCandidate struct {
	Name      string          `json:"name"`
	RowCount  int64           `json:"row_count"`
	RowGroups []RowGroupStats `json:"row_groups"`
}

// RowGroupStats describes one row group. When Pages is empty, Columns holds
// the statistics of the single implicit page that spans the whole group.
type RowGroupStats struct {
	RowCount int64             `json:"row_count"`
	Pages    []PageCostStats   `json:"pages"`
	Columns  []ColumnCostStats `json:"columns"`
}

// PageCostStats describes one page: its row count and one entry per column.
type PageCostStats struct {
	RowCount int64             `json:"row_count"`
	Columns  []ColumnCostStats `json:"columns"`
}

// ColumnCostStats is the per-column statistical summary the estimator prices:
// the encoded byte count, the null count, the distinct value count and the
// minimum and maximum where they are available. A nil DistinctCount, Min or
// Max marks the statistic as missing; missing statistics never lower the
// estimate, they flag it as incomplete instead.
type ColumnCostStats struct {
	Column        string `json:"column"`
	Type          string `json:"type"`
	Bytes         int64  `json:"bytes"`
	NullCount     int64  `json:"null_count"`
	DistinctCount *int64 `json:"distinct_count"`
	Min           *Value `json:"min"`
	Max           *Value `json:"max"`
}

// CandidateEstimate is the priced scan of one candidate, in input order.
type CandidateEstimate struct {
	Name                 string  `json:"name"`
	RowGroups            int     `json:"row_groups"`
	Pages                int     `json:"pages"`
	Bytes                int64   `json:"bytes"`
	DecodedValues        int64   `json:"decoded_values"`
	ProcessedRows        int64   `json:"processed_rows"`
	FilteredRows         float64 `json:"filtered_rows"`
	ReturnRows           float64 `json:"return_rows"`
	Cost                 float64 `json:"cost"`
	Reason               string  `json:"reason"`
	IncompleteStatistics bool    `json:"incomplete_statistics"`
}

// EstimateResult is the output of EstimateScan: one estimate per candidate in
// input order plus the index (and name) of the cheapest candidate. Selected
// is -1 when no candidate is chosen, which only happens for a zero limit.
type EstimateResult struct {
	Candidates   []CandidateEstimate `json:"candidates"`
	Selected     int                 `json:"selected"`
	SelectedName string              `json:"selected_name"`
}

// EstimateScan prices every candidate source for a scan and picks the
// cheapest one. It is pure: it reads only the statistics carried by the
// request, never candidate data, and it never writes or rewrites a file. The
// same request always produces the same result, and a cost tie is broken in
// favour of the candidate that comes first in the input.
//
// Each candidate is priced by excluding the columns neither the projection
// nor a predicate needs, pruning the pages whose statistics cannot satisfy a
// predicate, and then charging for the surviving bytes, decoded values and
// unpruned rows. A positive limit (or a larger sort requirement) stops the
// scan as soon as the estimated matching rows cover it.
func EstimateScan(req EstimateRequest) (EstimateResult, error) {
	if err := validateEstimateRequest(req); err != nil {
		return EstimateResult{}, err
	}
	if req.Limit != nil && *req.Limit == 0 {
		// A zero limit needs no scan: every candidate costs nothing and none
		// is selected.
		estimates := make([]CandidateEstimate, len(req.Candidates))
		for i, candidate := range req.Candidates {
			estimates[i] = CandidateEstimate{Name: candidate.Name, Reason: "limit is zero: nothing to scan"}
		}
		return EstimateResult{Candidates: estimates, Selected: -1}, nil
	}
	neededRows := int64(0)
	if req.Limit != nil {
		neededRows = *req.Limit
	}
	if req.SortRows != nil && *req.SortRows > neededRows {
		neededRows = *req.SortRows
	}
	estimates := make([]CandidateEstimate, len(req.Candidates))
	for i, candidate := range req.Candidates {
		estimates[i] = estimateCandidate(candidate, req, neededRows)
	}
	best := 0
	for i := 1; i < len(estimates); i++ {
		if estimates[i].Cost < estimates[best].Cost {
			best = i
		}
	}
	for i := range estimates {
		switch {
		case i == best:
			estimates[i].Reason = "selected: lowest cost"
		case estimates[i].Cost == estimates[best].Cost:
			estimates[i].Reason = fmt.Sprintf("not selected: same cost as candidate %d, which comes first", best)
		default:
			estimates[i].Reason = fmt.Sprintf("not selected: cost %.6f exceeds candidate %d at %.6f", estimates[i].Cost, best, estimates[best].Cost)
		}
	}
	return EstimateResult{
		Candidates:   estimates,
		Selected:     best,
		SelectedName: req.Candidates[best].Name,
	}, nil
}

// estimateCandidate prices one candidate. Counters only cover the row groups
// the scan would actually read: once the estimated matching rows reach
// neededRows the remaining groups are never touched. FilteredRows is the
// estimate over every surviving page of the candidate, independent of that
// early stop, because it describes the data rather than the scan.
func estimateCandidate(candidate SourceCandidate, req EstimateRequest, neededRows int64) CandidateEstimate {
	estimate := CandidateEstimate{Name: candidate.Name}
	needed := neededColumns(req, candidate)
	var filtered float64
	var accumulated float64
	stopped := false
	for _, group := range candidate.RowGroups {
		type pageOutcome struct {
			page    PageCostStats
			matched float64
		}
		var surviving []pageOutcome
		var groupMatched float64
		for _, page := range groupPages(group) {
			matched, pruned := estimatePage(page, req.Predicates, &estimate.IncompleteStatistics)
			if pruned {
				continue
			}
			surviving = append(surviving, pageOutcome{page, matched})
			groupMatched += matched
		}
		filtered += groupMatched
		if stopped || len(surviving) == 0 {
			continue
		}
		estimate.RowGroups++
		for _, outcome := range surviving {
			estimate.Pages++
			estimate.ProcessedRows += outcome.page.RowCount
			present := map[string]bool{}
			for _, chunk := range outcome.page.Columns {
				present[chunk.Column] = true
				if !needed[chunk.Column] {
					continue
				}
				estimate.Bytes += chunk.Bytes
				estimate.DecodedValues += outcome.page.RowCount
			}
			for column := range needed {
				if !present[column] {
					// A needed column without statistics cannot be priced.
					estimate.IncompleteStatistics = true
				}
			}
		}
		accumulated += groupMatched
		if neededRows > 0 && accumulated >= float64(neededRows) {
			stopped = true
		}
	}
	estimate.FilteredRows = filtered
	estimate.ReturnRows = filtered
	if req.Limit != nil && filtered > float64(*req.Limit) {
		estimate.ReturnRows = float64(*req.Limit)
	}
	estimate.Cost = float64(estimate.Bytes)/1000*costPerKilobyte +
		float64(estimate.DecodedValues)*costPerValue +
		float64(estimate.ProcessedRows)*costPerRow
	return estimate
}

// groupPages lists the pages of a row group. A group that declares no pages
// is itself the estimation unit, so its column statistics become one page.
func groupPages(group RowGroupStats) []PageCostStats {
	if len(group.Pages) > 0 {
		return group.Pages
	}
	return []PageCostStats{{RowCount: group.RowCount, Columns: group.Columns}}
}

// estimatePage decides whether a page survives predicate pruning and, when it
// does, estimates how many of its rows match. Selectivities multiply: the
// predicates are ANDed and the model assumes they are independent.
func estimatePage(page PageCostStats, predicates []Predicate, incomplete *bool) (matched float64, pruned bool) {
	rows := page.RowCount
	if rows == 0 {
		return 0, true
	}
	selectivity := 1.0
	for _, predicate := range predicates {
		chunk := findColumnStats(page.Columns, predicate.Column)
		if chunk == nil {
			// Without column statistics the predicate can neither prune nor
			// be priced, so it keeps selectivity 1.
			*incomplete = true
			continue
		}
		eliminated, sel, missing := predicateEstimate(predicate, rows, *chunk)
		if missing {
			*incomplete = true
		}
		if eliminated {
			return 0, true
		}
		selectivity *= sel
	}
	return float64(rows) * selectivity, false
}

// predicateEstimate applies one predicate to one column's page statistics and
// returns whether the page is provably eliminated, the predicate's
// selectivity when it is not, and whether missing statistics forced the
// conservative answer. Missing statistics always price as selectivity 1, the
// most expensive honest guess.
func predicateEstimate(predicate Predicate, rows int64, chunk ColumnCostStats) (pruned bool, selectivity float64, incomplete bool) {
	nulls := chunk.NullCount
	nonNull := rows - nulls
	switch predicate.Op {
	case OpIsNull:
		if nulls == 0 {
			return true, 0, false
		}
		return false, float64(nulls) / float64(rows), false
	case OpIsNotNull:
		if nonNull == 0 {
			return true, 0, false
		}
		return false, float64(nonNull) / float64(rows), false
	}
	// Equality and range predicates never match NULL, so an all-NULL page is
	// pruned no matter what the boundaries say.
	if nonNull == 0 {
		return true, 0, false
	}
	nonNullRatio := float64(nonNull) / float64(rows)
	stats := PageStats{Nulls: nulls}
	bounded := false
	if chunk.Min != nil && chunk.Max != nil && !chunk.Min.Null && !chunk.Max.Null {
		stats.Set, stats.Min, stats.Max = true, *chunk.Min, *chunk.Max
		bounded = true
	}
	// Pruning reuses the query engine's own rule, so the estimate agrees with
	// the pages a real scan would skip.
	if !predicate.pageMatches(stats, rows) {
		return true, 0, false
	}
	if predicate.Op == OpEq {
		if chunk.DistinctCount == nil {
			return false, 1, true
		}
		if *chunk.DistinctCount <= 0 {
			return false, 1, !bounded
		}
		sel := nonNullRatio / float64(*chunk.DistinctCount)
		return false, math.Min(sel, 1), !bounded
	}
	// A range predicate is priced by the overlap of its interval with the
	// recorded [min, max], scaled by the non-null ratio.
	if !bounded {
		return false, 1, true
	}
	mn, okMin := chunk.Min.Number()
	mx, okMax := chunk.Max.Number()
	if !okMin || !okMax {
		return false, 1, true
	}
	lo, hi, ok := predicateRange(predicate)
	if !ok {
		return false, 1, true
	}
	ratio := 1.0
	if mx > mn {
		ratio = (math.Min(hi, mx) - math.Max(lo, mn)) / (mx - mn)
		ratio = math.Max(0, math.Min(1, ratio))
	}
	return false, nonNullRatio * ratio, false
}

// predicateRange renders a range predicate as an interval. The bounds are
// treated as closed; the open or closed edge of < and > is negligible for an
// estimate.
func predicateRange(predicate Predicate) (float64, float64, bool) {
	switch predicate.Op {
	case OpLt, OpLte:
		hi, ok := predicate.Value.Number()
		return math.Inf(-1), hi, ok
	case OpGt, OpGte:
		lo, ok := predicate.Value.Number()
		return lo, math.Inf(1), ok
	case OpBetween:
		lo, okLo := predicate.Lo.Number()
		hi, okHi := predicate.Hi.Number()
		return lo, hi, okLo && okHi
	}
	return 0, 0, false
}

// neededColumns resolves which columns the scan must decode: the projection
// and the predicate columns, narrowed to the requested read columns. An empty
// projection reads every requested column, mirroring the query engine, which
// defaults an empty projection to the whole schema.
func neededColumns(req EstimateRequest, candidate SourceCandidate) map[string]bool {
	predicateColumns := map[string]bool{}
	for _, predicate := range req.Predicates {
		predicateColumns[predicate.Column] = true
	}
	projection := req.Projection
	if len(projection) == 0 {
		projection = req.ReadColumns
	}
	needed := map[string]bool{}
	for _, name := range projection {
		needed[name] = true
	}
	for name := range predicateColumns {
		needed[name] = true
	}
	if len(req.ReadColumns) > 0 {
		read := map[string]bool{}
		for _, name := range req.ReadColumns {
			read[name] = true
		}
		for name := range needed {
			if !read[name] && !predicateColumns[name] {
				delete(needed, name)
			}
		}
	}
	if len(needed) == 0 && len(req.Projection) == 0 && len(req.Predicates) == 0 {
		// No projection and no read list at all: every column the candidate
		// describes is read, like the engine defaulting to the whole schema.
		for _, group := range candidate.RowGroups {
			for _, page := range groupPages(group) {
				for _, chunk := range page.Columns {
					needed[chunk.Column] = true
				}
			}
		}
	}
	return needed
}

func findColumnStats(chunks []ColumnCostStats, column string) *ColumnCostStats {
	for i := range chunks {
		if chunks[i].Column == column {
			return &chunks[i]
		}
	}
	return nil
}

// validateEstimateRequest enforces the estimator's input contract before any
// candidate is priced, so a rejected request never yields a partial estimate.
func validateEstimateRequest(req EstimateRequest) error {
	if len(req.Candidates) == 0 {
		return invalidArgumentf("at least one candidate source is required")
	}
	if req.Limit != nil && *req.Limit < 0 {
		return invalidArgumentf("limit must not be negative")
	}
	if req.SortRows != nil && *req.SortRows < 0 {
		return invalidArgumentf("sort_rows must not be negative")
	}
	for _, predicate := range req.Predicates {
		if err := validateEstimatePredicate(predicate); err != nil {
			return err
		}
	}
	for i, candidate := range req.Candidates {
		if candidate.RowCount < 0 {
			return invalidArgumentf("candidate %d has a negative row count %d", i, candidate.RowCount)
		}
		for j, group := range candidate.RowGroups {
			where := fmt.Sprintf("candidate %d row group %d", i, j)
			if group.RowCount < 0 {
				return invalidArgumentf("%s has a negative row count %d", where, group.RowCount)
			}
			if len(group.Pages) == 0 {
				if err := validateColumnStats(group.Columns, group.RowCount, where); err != nil {
					return err
				}
			}
			for k, page := range group.Pages {
				pageWhere := fmt.Sprintf("%s page %d", where, k)
				if page.RowCount < 0 {
					return invalidArgumentf("%s has a negative row count %d", pageWhere, page.RowCount)
				}
				if err := validateColumnStats(page.Columns, page.RowCount, pageWhere); err != nil {
					return err
				}
			}
		}
	}
	// A range predicate can only be priced against a comparable numeric (or
	// date-like) interval; anything else is rejected up front rather than
	// estimated halfway.
	for _, predicate := range req.Predicates {
		if !isRangeOperator(predicate.Op) {
			continue
		}
		for i, candidate := range req.Candidates {
			typ := candidateColumnType(candidate, predicate.Column)
			if typ != "" && typ != TypeInt64 && typ != TypeFloat64 {
				return unsupportedPredicatef("range predicate on %s column %q is not supported by the cost model (candidate %d)", typ, predicate.Column, i)
			}
		}
	}
	return nil
}

func validateEstimatePredicate(predicate Predicate) error {
	if strings.TrimSpace(predicate.Column) == "" {
		return invalidArgumentf("predicate column is required")
	}
	switch predicate.Op {
	case OpEq, OpLt, OpLte, OpGt, OpGte:
		if predicate.Value == nil {
			return invalidArgumentf("predicate %q on %q requires a value", predicate.Op, predicate.Column)
		}
	case OpBetween:
		if predicate.Lo == nil || predicate.Hi == nil {
			return invalidArgumentf("between on %q requires lo and hi", predicate.Column)
		}
		if compareValues(*predicate.Lo, *predicate.Hi) > 0 {
			return invalidArgumentf("between lo must not exceed hi")
		}
	case OpIsNull, OpIsNotNull:
		// NULL tests carry no comparison value.
	default:
		return unsupportedPredicatef("unsupported predicate operator %q", predicate.Op)
	}
	return nil
}

func validateColumnStats(chunks []ColumnCostStats, rows int64, where string) error {
	for _, chunk := range chunks {
		if chunk.Bytes < 0 {
			return invalidArgumentf("%s: column %q has a negative byte count %d", where, chunk.Column, chunk.Bytes)
		}
		if chunk.NullCount < 0 {
			return invalidArgumentf("%s: column %q has a negative null count %d", where, chunk.Column, chunk.NullCount)
		}
		if chunk.NullCount > rows {
			return invalidArgumentf("%s: column %q null count %d exceeds the page row count %d", where, chunk.Column, chunk.NullCount, rows)
		}
		if chunk.DistinctCount != nil {
			if *chunk.DistinctCount < 0 {
				return invalidArgumentf("%s: column %q has a negative distinct count %d", where, chunk.Column, *chunk.DistinctCount)
			}
			if *chunk.DistinctCount > rows-chunk.NullCount {
				return invalidArgumentf("%s: column %q distinct count %d exceeds the non-null row count %d", where, chunk.Column, *chunk.DistinctCount, rows-chunk.NullCount)
			}
		}
		if chunk.Min != nil && chunk.Max != nil && !chunk.Min.Null && !chunk.Max.Null && compareValues(*chunk.Min, *chunk.Max) > 0 {
			return invalidArgumentf("%s: column %q maximum is below its minimum", where, chunk.Column)
		}
	}
	return nil
}

func isRangeOperator(op string) bool {
	switch op {
	case OpLt, OpLte, OpGt, OpGte, OpBetween:
		return true
	}
	return false
}

// candidateColumnType reports the declared type of a column inside one
// candidate, falling back to the kind of its recorded boundaries. An empty
// result means the candidate says nothing about the column's type.
func candidateColumnType(candidate SourceCandidate, column string) string {
	for _, group := range candidate.RowGroups {
		for _, page := range groupPages(group) {
			for _, chunk := range page.Columns {
				if chunk.Column != column {
					continue
				}
				if chunk.Type != "" {
					return chunk.Type
				}
				if chunk.Min != nil && chunk.Min.Kind != "" {
					return chunk.Min.Kind
				}
				if chunk.Max != nil && chunk.Max.Kind != "" {
					return chunk.Max.Kind
				}
			}
		}
	}
	return ""
}
