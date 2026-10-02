package tests

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"columna/internal/columna"
)

func int64Ptr(v int64) *int64 { return &v }

func valuePtr(v columna.Value) *columna.Value { return &v }

// statsCandidate builds a one-group, one-page candidate whose single column
// carries the given statistics.
func statsCandidate(name string, rows int64, chunk columna.ColumnCostStats) columna.SourceCandidate {
	return columna.SourceCandidate{
		Name:     name,
		RowCount: rows,
		RowGroups: []columna.RowGroupStats{{
			RowCount: rows,
			Columns:  []columna.ColumnCostStats{chunk},
		}},
	}
}

func intChunk(column string, bytes, nulls, distinct, min, max int64) columna.ColumnCostStats {
	return columna.ColumnCostStats{
		Column:        column,
		Type:          columna.TypeInt64,
		Bytes:         bytes,
		NullCount:     nulls,
		DistinctCount: int64Ptr(distinct),
		Min:           valuePtr(columna.IntValue(min)),
		Max:           valuePtr(columna.IntValue(max)),
	}
}

func eqPredicate(column string, value int64) columna.Predicate {
	v := columna.IntValue(value)
	return columna.Predicate{Column: column, Op: columna.OpEq, Value: &v}
}

func TestEstimateEqualitySelectivityAndCost(t *testing.T) {
	req := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{statsCandidate("a", 100, intChunk("id", 800, 0, 10, 1, 100))},
		Projection: []string{"id"},
		Predicates: []columna.Predicate{eqPredicate("id", 5)},
	}
	result, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if result.Selected != 0 || result.SelectedName != "a" {
		t.Fatalf("expected candidate 0 selected, got %d (%q)", result.Selected, result.SelectedName)
	}
	est := result.Candidates[0]
	if est.RowGroups != 1 || est.Pages != 1 {
		t.Fatalf("expected 1 row group and 1 page, got %d and %d", est.RowGroups, est.Pages)
	}
	if est.Bytes != 800 || est.DecodedValues != 100 || est.ProcessedRows != 100 {
		t.Fatalf("unexpected counters: %+v", est)
	}
	// selectivity = non-null ratio 1.0 / distinct 10
	if est.FilteredRows != 10 || est.ReturnRows != 10 {
		t.Fatalf("expected 10 filtered rows, got %v (return %v)", est.FilteredRows, est.ReturnRows)
	}
	// cost = 800/1000 * 1 + 100 * 0.01 + 100 * 0.001
	want := 0.8 + 1.0 + 0.1
	if math.Abs(est.Cost-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", est.Cost, want)
	}
	if est.IncompleteStatistics {
		t.Fatal("complete statistics must not be flagged incomplete")
	}
	if est.Reason == "" {
		t.Fatal("a selection reason is required")
	}
}

func TestEstimatePrunesImpossiblePages(t *testing.T) {
	req := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{statsCandidate("a", 100, intChunk("id", 800, 0, 10, 1, 100))},
		Projection: []string{"id"},
		Predicates: []columna.Predicate{eqPredicate("id", 500)},
	}
	result, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	est := result.Candidates[0]
	if est.RowGroups != 0 || est.Pages != 0 || est.Bytes != 0 || est.ProcessedRows != 0 {
		t.Fatalf("page outside [min, max] must be pruned: %+v", est)
	}
	if est.FilteredRows != 0 || est.Cost != 0 {
		t.Fatalf("pruned candidate must be free: %+v", est)
	}
}

func TestEstimateRangeSelectivity(t *testing.T) {
	lo, hi := columna.IntValue(1), columna.IntValue(50)
	req := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{statsCandidate("a", 100, intChunk("id", 800, 0, 100, 1, 100))},
		Projection: []string{"id"},
		Predicates: []columna.Predicate{{Column: "id", Op: columna.OpBetween, Lo: &lo, Hi: &hi}},
	}
	result, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	// overlap of [1, 50] with [1, 100] is (50-1)/(100-1) of the non-null rows
	want := 100 * 49.0 / 99.0
	if got := result.Candidates[0].FilteredRows; math.Abs(got-want) > 1e-9 {
		t.Fatalf("filtered rows = %v, want %v", got, want)
	}
}

func TestEstimateNullSelectivities(t *testing.T) {
	chunk := intChunk("score", 800, 25, 10, 1, 100)
	for _, test := range []struct {
		op   string
		want float64
	}{
		{columna.OpIsNull, 25},
		{columna.OpIsNotNull, 75},
	} {
		req := columna.EstimateRequest{
			Candidates: []columna.SourceCandidate{statsCandidate("a", 100, chunk)},
			Projection: []string{"score"},
			Predicates: []columna.Predicate{{Column: "score", Op: test.op}},
		}
		result, err := columna.EstimateScan(req)
		if err != nil {
			t.Fatalf("%s: %v", test.op, err)
		}
		if got := result.Candidates[0].FilteredRows; got != test.want {
			t.Fatalf("%s: filtered rows = %v, want %v", test.op, got, test.want)
		}
	}
}

func TestEstimateEqualityUsesNonNullRatio(t *testing.T) {
	// selectivity = (75/100) / 5
	req := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{statsCandidate("a", 100, intChunk("id", 800, 25, 5, 1, 100))},
		Projection: []string{"id"},
		Predicates: []columna.Predicate{eqPredicate("id", 5)},
	}
	result, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if got := result.Candidates[0].FilteredRows; got != 15 {
		t.Fatalf("filtered rows = %v, want 15", got)
	}
}

// groupedCandidate builds a candidate of n row groups of 100 rows each.
func groupedCandidate(name string, groups int) columna.SourceCandidate {
	candidate := columna.SourceCandidate{Name: name, RowCount: int64(100 * groups)}
	for i := 0; i < groups; i++ {
		candidate.RowGroups = append(candidate.RowGroups, columna.RowGroupStats{
			RowCount: 100,
			Columns:  []columna.ColumnCostStats{intChunk("id", 800, 0, 100, 1, 100)},
		})
	}
	return candidate
}

func TestEstimateLimitStopsEarly(t *testing.T) {
	req := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{groupedCandidate("a", 3)},
		Projection: []string{"id"},
		Limit:      int64Ptr(150),
	}
	result, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	est := result.Candidates[0]
	if est.RowGroups != 2 {
		t.Fatalf("limit 150 must stop after 2 row groups, got %d", est.RowGroups)
	}
	if est.FilteredRows != 300 {
		t.Fatalf("filtered rows describe the whole source: got %v, want 300", est.FilteredRows)
	}
	if est.ReturnRows != 150 {
		t.Fatalf("return rows must be capped by the limit: got %v", est.ReturnRows)
	}
}

func TestEstimateSortRowsExtendTheScan(t *testing.T) {
	req := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{groupedCandidate("a", 3)},
		Projection: []string{"id"},
		Limit:      int64Ptr(50),
		SortRows:   int64Ptr(250),
	}
	result, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	est := result.Candidates[0]
	if est.RowGroups != 3 {
		t.Fatalf("the sort needs 250 rows, so all 3 groups are read: got %d", est.RowGroups)
	}
	if est.ReturnRows != 50 {
		t.Fatalf("return rows are still capped by the limit: got %v", est.ReturnRows)
	}
}

func TestEstimateZeroLimitSelectsNothing(t *testing.T) {
	req := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{groupedCandidate("a", 2), groupedCandidate("b", 1)},
		Projection: []string{"id"},
		Predicates: []columna.Predicate{eqPredicate("id", 5)},
		Limit:      int64Ptr(0),
	}
	result, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if result.Selected != -1 {
		t.Fatalf("zero limit must not select a candidate, got %d", result.Selected)
	}
	for i, est := range result.Candidates {
		if est.Cost != 0 || est.RowGroups != 0 || est.ReturnRows != 0 {
			t.Fatalf("candidate %d must be free and empty: %+v", i, est)
		}
	}
}

func TestEstimatePicksCheapestAndBreaksTiesByOrder(t *testing.T) {
	cheap := statsCandidate("cheap", 100, intChunk("id", 400, 0, 10, 1, 100))
	dear := statsCandidate("dear", 100, intChunk("id", 4000, 0, 10, 1, 100))
	req := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{dear, cheap},
		Projection: []string{"id"},
	}
	result, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if result.Selected != 1 || result.SelectedName != "cheap" {
		t.Fatalf("the cheaper candidate must win, got %d (%q)", result.Selected, result.SelectedName)
	}
	if result.Candidates[0].Reason == "" || result.Candidates[1].Reason == "" {
		t.Fatal("every candidate needs a reason")
	}

	tie := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{cheap, cheap},
		Projection: []string{"id"},
	}
	tied, err := columna.EstimateScan(tie)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if tied.Selected != 0 {
		t.Fatalf("a cost tie must keep the first candidate, got %d", tied.Selected)
	}
}

func TestEstimateProjectionExcludesColumns(t *testing.T) {
	candidate := columna.SourceCandidate{
		Name:     "a",
		RowCount: 100,
		RowGroups: []columna.RowGroupStats{{
			RowCount: 100,
			Columns: []columna.ColumnCostStats{
				intChunk("id", 800, 0, 10, 1, 100),
				intChunk("payload", 16000, 0, 100, 1, 100),
			},
		}},
	}
	req := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{candidate},
		Projection: []string{"id"},
	}
	result, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	est := result.Candidates[0]
	if est.Bytes != 800 || est.DecodedValues != 100 {
		t.Fatalf("only the projected column is read: %+v", est)
	}

	// A predicate column is read even when it is not projected.
	req.Predicates = []columna.Predicate{eqPredicate("payload", 5)}
	withFilter, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if got := withFilter.Candidates[0].Bytes; got != 800+16000 {
		t.Fatalf("predicate columns are decoded too: bytes = %d", got)
	}
}

func TestEstimateMissingStatisticsAreIncomplete(t *testing.T) {
	// No distinct count: the equality selectivity falls back to 1.
	chunk := intChunk("id", 800, 0, 10, 1, 100)
	chunk.DistinctCount = nil
	req := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{statsCandidate("a", 100, chunk)},
		Projection: []string{"id"},
		Predicates: []columna.Predicate{eqPredicate("id", 5)},
	}
	result, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	est := result.Candidates[0]
	if !est.IncompleteStatistics {
		t.Fatal("missing distinct count must flag incomplete statistics")
	}
	if est.FilteredRows != 100 {
		t.Fatalf("missing statistics price as selectivity 1, got %v", est.FilteredRows)
	}

	// No min/max: a range predicate can neither prune nor be priced.
	unbounded := intChunk("id", 800, 0, 10, 1, 100)
	unbounded.Min, unbounded.Max = nil, nil
	v := columna.IntValue(50)
	rangeReq := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{statsCandidate("a", 100, unbounded)},
		Projection: []string{"id"},
		Predicates: []columna.Predicate{{Column: "id", Op: columna.OpGt, Value: &v}},
	}
	ranged, err := columna.EstimateScan(rangeReq)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if !ranged.Candidates[0].IncompleteStatistics || ranged.Candidates[0].FilteredRows != 100 {
		t.Fatalf("missing boundaries must be conservative: %+v", ranged.Candidates[0])
	}
}

func TestEstimateValidation(t *testing.T) {
	valid := func() columna.EstimateRequest {
		return columna.EstimateRequest{
			Candidates: []columna.SourceCandidate{statsCandidate("a", 100, intChunk("id", 800, 0, 10, 1, 100))},
			Projection: []string{"id"},
		}
	}
	var invalid *columna.InvalidArgumentError
	cases := map[string]func(*columna.EstimateRequest){
		"no candidates": func(r *columna.EstimateRequest) { r.Candidates = nil },
		"negative rows": func(r *columna.EstimateRequest) { r.Candidates[0].RowCount = -1 },
		"negative group rows": func(r *columna.EstimateRequest) {
			r.Candidates[0].RowGroups[0].RowCount = -1
		},
		"negative bytes": func(r *columna.EstimateRequest) {
			r.Candidates[0].RowGroups[0].Columns[0].Bytes = -1
		},
		"negative nulls": func(r *columna.EstimateRequest) {
			r.Candidates[0].RowGroups[0].Columns[0].NullCount = -1
		},
		"nulls above rows": func(r *columna.EstimateRequest) {
			r.Candidates[0].RowGroups[0].Columns[0].NullCount = 101
		},
		"max below min": func(r *columna.EstimateRequest) {
			r.Candidates[0].RowGroups[0].Columns[0].Max = valuePtr(columna.IntValue(0))
		},
		"distinct above non-null": func(r *columna.EstimateRequest) {
			r.Candidates[0].RowGroups[0].Columns[0].NullCount = 50
			r.Candidates[0].RowGroups[0].Columns[0].DistinctCount = int64Ptr(51)
		},
		"negative limit": func(r *columna.EstimateRequest) { r.Limit = int64Ptr(-1) },
		"missing value": func(r *columna.EstimateRequest) {
			r.Predicates = []columna.Predicate{{Column: "id", Op: columna.OpEq}}
		},
		"between inverted": func(r *columna.EstimateRequest) {
			lo, hi := columna.IntValue(9), columna.IntValue(1)
			r.Predicates = []columna.Predicate{{Column: "id", Op: columna.OpBetween, Lo: &lo, Hi: &hi}}
		},
	}
	for name, mutate := range cases {
		req := valid()
		mutate(&req)
		_, err := columna.EstimateScan(req)
		if !errors.As(err, &invalid) {
			t.Fatalf("%s: expected InvalidArgumentError, got %v", name, err)
		}
	}
}

func TestEstimateUnsupportedPredicates(t *testing.T) {
	var unsupported *columna.UnsupportedPredicateError
	base := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{statsCandidate("a", 100, intChunk("id", 800, 0, 10, 1, 100))},
		Projection: []string{"id"},
	}

	unknown := base
	v := columna.IntValue(1)
	unknown.Predicates = []columna.Predicate{{Column: "id", Op: "!=", Value: &v}}
	if _, err := columna.EstimateScan(unknown); !errors.As(err, &unsupported) {
		t.Fatalf("unknown operator: expected UnsupportedPredicateError, got %v", err)
	}

	// A range predicate on a string column cannot be priced.
	stringChunk := columna.ColumnCostStats{
		Column: "region", Type: columna.TypeString, Bytes: 500,
		Min: valuePtr(columna.StringValue("east")),
		Max: valuePtr(columna.StringValue("west")),
	}
	strings_ := base
	strings_.Candidates = []columna.SourceCandidate{statsCandidate("a", 100, stringChunk)}
	strings_.Predicates = []columna.Predicate{{Column: "region", Op: columna.OpGt, Value: valuePtr(columna.StringValue("m"))}}
	if _, err := columna.EstimateScan(strings_); !errors.As(err, &unsupported) {
		t.Fatalf("string range: expected UnsupportedPredicateError, got %v", err)
	}
}

func TestEstimateIsDeterministic(t *testing.T) {
	req := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{
			groupedCandidate("a", 3),
			statsCandidate("b", 100, intChunk("id", 400, 0, 10, 1, 100)),
		},
		ReadColumns: []string{"id"},
		Projection:  []string{"id"},
		Predicates:  []columna.Predicate{eqPredicate("id", 5)},
		Limit:       int64Ptr(150),
	}
	first, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	second, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("identical requests must produce identical estimates")
	}
}

func TestEstimateReasonMentionsSelection(t *testing.T) {
	req := columna.EstimateRequest{
		Candidates: []columna.SourceCandidate{
			statsCandidate("a", 100, intChunk("id", 800, 0, 10, 1, 100)),
			statsCandidate("b", 100, intChunk("id", 400, 0, 10, 1, 100)),
		},
		Projection: []string{"id"},
	}
	result, err := columna.EstimateScan(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if !strings.Contains(result.Candidates[result.Selected].Reason, "lowest cost") {
		t.Fatalf("the selected candidate must say why: %q", result.Candidates[result.Selected].Reason)
	}
}
