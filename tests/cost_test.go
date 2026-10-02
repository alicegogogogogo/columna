package tests

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"columna/internal/columna"
)

// ---- candidate builders ------------------------------------------------------

func intStat(rows, nulls, distinct int64, min, max int64) columna.PageStat {
	return columna.PageStat{
		Rows: rows, Nulls: nulls, Distinct: distinct,
		Set: true, Min: columna.IntValue(min), Max: columna.IntValue(max),
	}
}

// tenRowsCandidate is one row group of ten rows in two pages of five rows for
// the int64 column "id"; pages cover [1,5] and [6,10].
func tenRowsCandidate(name string) columna.CandidateSource {
	return columna.CandidateSource{
		Name: name,
		Rows: 10,
		Groups: []columna.GroupStat{{
			Rows: 10,
			Pages: map[string][]columna.PageStat{
				"id": {
					intStat(5, 0, 5, 1, 5),
					intStat(5, 0, 5, 6, 10),
				},
			},
		}},
		Columns: map[string]columna.ColumnInput{
			"id": {
				Bytes:    []int64{100, 200},
				Nulls:    0,
				Distinct: 10,
				Set:      true,
				Min:      columna.IntValue(1),
				Max:      columna.IntValue(10),
			},
		},
	}
}

func intPredicate(column, op string, value int64) columna.CostPredicate {
	v := columna.IntValue(value)
	return columna.CostPredicate{Column: column, Type: columna.TypeInt64, Op: op, Value: &v}
}

func i64(v int64) *int64 { return &v }

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("cost = %.12g, want %.12g", got, want)
	}
}

func estimate(t *testing.T, req columna.ScanRequest) columna.ScanCostResult {
	t.Helper()
	result, err := columna.EstimateScanCost(req)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	return result
}

// ---- formulas ---------------------------------------------------------------

func TestCostWithoutPredicates(t *testing.T) {
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{tenRowsCandidate("a")},
		ReadColumns: []string{"id"},
	})
	c := result.Candidates[0]
	if result.Selected != "a" {
		t.Fatalf("selected = %q, want a", result.Selected)
	}
	if c.RowGroups != 1 || c.Pages != 2 || c.Bytes != 300 || c.DecodedValues != 10 {
		t.Fatalf("unexpected scan size: %+v", c)
	}
	if c.FilteredRows != 10 || c.ReturnedRows != 10 || c.ProcessedRows != 10 {
		t.Fatalf("unexpected row counts: %+v", c)
	}
	// 300/1000 + 0.01*10 + 0.001*10 = 0.41
	approx(t, c.Cost, 0.41)
}

func TestEqualitySelectivityPrunesAndFilters(t *testing.T) {
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{tenRowsCandidate("a")},
		ReadColumns: []string{"id"},
		Predicates:  []columna.CostPredicate{intPredicate("id", columna.OpEq, 3)},
	})
	c := result.Candidates[0]
	// Page 1 ([6,10]) is pruned; page 0 survives with selectivity 1/5.
	if c.Pages != 1 || c.Bytes != 100 || c.DecodedValues != 5 {
		t.Fatalf("unexpected scan size: %+v", c)
	}
	if c.FilteredRows != 1 {
		t.Fatalf("filtered = %d, want 1", c.FilteredRows)
	}
	if c.IncompleteStatistics {
		t.Fatalf("statistics are complete: distinct counts present")
	}
	// 100/1000 + 0.01*5 + 0.001*5 = 0.155
	approx(t, c.Cost, 0.155)
}

func TestRangeSelectivityUsesIntervalOverlap(t *testing.T) {
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{tenRowsCandidate("a")},
		ReadColumns: []string{"id"},
		Predicates: []columna.CostPredicate{
			{Column: "id", Type: columna.TypeInt64, Op: columna.OpBetween,
				Lo: ptr(columna.IntValue(3)), Hi: ptr(columna.IntValue(7))},
		},
	})
	c := result.Candidates[0]
	// Page [1,5] overlaps [3,7] over 2/4 of its span (2.5 rows) and page
	// [6,10] over 1/4 (1.25 rows): 3.75 rows, rounded to four.
	if c.FilteredRows != 4 || c.Bytes != 300 {
		t.Fatalf("unexpected estimate: %+v", c)
	}
	approx(t, c.Cost, 0.41)
}

func TestFloatRangeSelectivity(t *testing.T) {
	candidate := columna.CandidateSource{
		Name: "f", Rows: 10,
		Groups: []columna.GroupStat{{
			Rows: 10,
			Pages: map[string][]columna.PageStat{
				"v": {{Rows: 10, Distinct: 10, Set: true,
					Min: columna.FloatValue(0), Max: columna.FloatValue(10)}},
			},
		}},
		Columns: map[string]columna.ColumnInput{
			"v": {Bytes: []int64{80}, Distinct: 10, Set: true,
				Min: columna.FloatValue(0), Max: columna.FloatValue(10)},
		},
	}
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{candidate},
		ReadColumns: []string{"v"},
		Predicates: []columna.CostPredicate{{
			Column: "v", Type: columna.TypeFloat64, Op: columna.OpBetween,
			Lo: ptr(columna.FloatValue(2)), Hi: ptr(columna.FloatValue(4)),
		}},
	})
	if got := result.Candidates[0].FilteredRows; got != 2 {
		t.Fatalf("filtered = %d, want 2", got)
	}
}

func TestNullSelectivity(t *testing.T) {
	candidate := columna.CandidateSource{
		Name: "n", Rows: 10,
		Groups: []columna.GroupStat{{
			Rows: 10,
			Pages: map[string][]columna.PageStat{
				"x": {
					{Rows: 5, Nulls: 5}, // all NULL, no boundaries
					intStat(5, 0, 5, 1, 5),
				},
			},
		}},
		Columns: map[string]columna.ColumnInput{
			"x": {Bytes: []int64{10, 10}, Nulls: 5, Distinct: 5, Set: true,
				Min: columna.IntValue(1), Max: columna.IntValue(5)},
		},
	}
	isNull := columna.CostPredicate{Column: "x", Type: columna.TypeInt64, IsNull: true}
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{candidate},
		ReadColumns: []string{"x"},
		Predicates:  []columna.CostPredicate{isNull},
	})
	c := result.Candidates[0]
	// The non-NULL page is pruned; the all-NULL page matches all five rows.
	if c.Pages != 1 || c.FilteredRows != 5 {
		t.Fatalf("IS NULL estimate: %+v", c)
	}

	isNotNull := columna.CostPredicate{Column: "x", Type: columna.TypeInt64, IsNotNull: true}
	result = estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{candidate},
		ReadColumns: []string{"x"},
		Predicates:  []columna.CostPredicate{isNotNull},
	})
	c = result.Candidates[0]
	// The all-NULL page is pruned; page 0 has five non-NULL rows.
	if c.Pages != 1 || c.FilteredRows != 5 {
		t.Fatalf("IS NOT NULL estimate: %+v", c)
	}
}

func TestPrunedCandidateHasZeroCost(t *testing.T) {
	cheap := tenRowsCandidate("small")
	big := tenRowsCandidate("big")
	big.Columns["id"] = columna.ColumnInput{
		Bytes: []int64{1000, 2000}, Distinct: 10, Set: true,
		Min: columna.IntValue(100), Max: columna.IntValue(110),
	}
	big.Groups[0].Pages["id"] = []columna.PageStat{
		intStat(5, 0, 5, 100, 104),
		intStat(5, 0, 5, 105, 110),
	}
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{big, cheap},
		ReadColumns: []string{"id"},
		Predicates:  []columna.CostPredicate{intPredicate("id", columna.OpEq, 3)},
	})
	if result.Candidates[0].Cost != 0 || result.Candidates[0].ProcessedRows != 0 {
		t.Fatalf("fully pruned candidate must cost zero: %+v", result.Candidates[0])
	}
	if result.Selected != "big" {
		t.Fatalf("selected = %q, want the zero-cost candidate big", result.Selected)
	}
}

func TestLimitEarlyStopAndCap(t *testing.T) {
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{tenRowsCandidate("a")},
		ReadColumns: []string{"id"},
		Limit:       i64(3),
	})
	c := result.Candidates[0]
	// Page granularity: the first five-row page is enough for three rows, the
	// second page is never read.
	if c.Bytes != 100 || c.DecodedValues != 5 || c.ProcessedRows != 5 {
		t.Fatalf("early stop size: %+v", c)
	}
	if c.ReturnedRows != 3 || c.FilteredRows != 10 {
		t.Fatalf("returned/filtered: %+v", c)
	}

	zero := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{tenRowsCandidate("a")},
		ReadColumns: []string{"id"},
		Limit:       i64(0),
	})
	if zero.Selected != "" {
		t.Fatalf("limit zero must select nobody, got %q", zero.Selected)
	}
	for _, c := range zero.Candidates {
		if c.Cost != 0 || c.Bytes != 0 || c.ReturnedRows != 0 {
			t.Fatalf("limit zero must be zero cost: %+v", c)
		}
	}
}

func TestSortRowsDelaysEarlyStop(t *testing.T) {
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{tenRowsCandidate("a")},
		ReadColumns: []string{"id"},
		Limit:       i64(1),
		SortRows:    6,
	})
	c := result.Candidates[0]
	// The sorter needs six filtered rows; the first five-row page only yields
	// five, so the walk continues into the second page.
	if c.Bytes != 300 || c.ProcessedRows != 10 || c.ReturnedRows != 1 {
		t.Fatalf("sort early stop: %+v", c)
	}
}

func TestMissingStatisticsAreConservative(t *testing.T) {
	candidate := columna.CandidateSource{
		Name: "m", Rows: 10,
		Groups: []columna.GroupStat{{
			Rows: 10,
			Pages: map[string][]columna.PageStat{
				"id": {{Rows: 10}}, // no distinct count, no boundaries
			},
		}},
		Columns: map[string]columna.ColumnInput{
			"id": {Bytes: []int64{100}},
		},
	}
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{candidate},
		ReadColumns: []string{"id"},
		Predicates:  []columna.CostPredicate{intPredicate("id", columna.OpEq, 3)},
	})
	c := result.Candidates[0]
	if !c.IncompleteStatistics {
		t.Fatalf("missing distinct count must flag incomplete statistics")
	}
	if c.FilteredRows != 10 || c.Cost == 0 {
		t.Fatalf("missing statistics must use selectivity one, got %+v", c)
	}
}

func TestProjectionExcludesColumnsFromCost(t *testing.T) {
	candidate := tenRowsCandidate("a")
	candidate.Columns["other"] = columna.ColumnInput{
		Bytes: []int64{900, 900}, Distinct: 1, Set: true,
		Min: columna.IntValue(1), Max: columna.IntValue(1),
	}
	candidate.Groups[0].Pages["other"] = []columna.PageStat{
		intStat(5, 0, 1, 1, 1),
		intStat(5, 0, 1, 1, 1),
	}
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{candidate},
		ReadColumns: []string{"id"},
	})
	if c := result.Candidates[0]; c.Bytes != 300 {
		t.Fatalf("unread column must not count bytes, got %d", c.Bytes)
	}
}

func TestTieGoesToFirstCandidate(t *testing.T) {
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{tenRowsCandidate("first"), tenRowsCandidate("second")},
		ReadColumns: []string{"id"},
	})
	if result.Selected != "first" {
		t.Fatalf("selected = %q, want first on a tie", result.Selected)
	}
}

func TestEstimateIsDeterministic(t *testing.T) {
	req := columna.ScanRequest{
		Sources:     []columna.CandidateSource{tenRowsCandidate("a"), tenRowsCandidate("b")},
		ReadColumns: []string{"id"},
		Predicates:  []columna.CostPredicate{intPredicate("id", columna.OpGte, 6)},
		Limit:       i64(2),
	}
	first := estimate(t, req)
	second := estimate(t, req)
	if !deepEqual(first, second) {
		t.Fatalf("estimates differ:\n%+v\n%+v", first, second)
	}
}

// ---- boundary-free sources --------------------------------------------------

func TestBoundaryFreeSource(t *testing.T) {
	candidate := columna.CandidateSource{
		Name: "loose", Rows: 10,
		Columns: map[string]columna.ColumnInput{
			"id": {Bytes: []int64{100, 200}, Distinct: 10, Set: true,
				Min: columna.IntValue(1), Max: columna.IntValue(10)},
		},
	}
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{candidate},
		ReadColumns: []string{"id"},
		Predicates:  []columna.CostPredicate{intPredicate("id", columna.OpEq, 3)},
	})
	c := result.Candidates[0]
	// Candidate-wide selectivity 1/10; physical page count and bytes are still
	// reported, and bytes are never read partially without page boundaries.
	if c.RowGroups != 1 || c.Pages != 2 || c.Bytes != 300 || c.DecodedValues != 10 {
		t.Fatalf("boundary-free size: %+v", c)
	}
	if c.FilteredRows != 1 {
		t.Fatalf("boundary-free filtered = %d, want 1", c.FilteredRows)
	}

	pruned := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{candidate},
		ReadColumns: []string{"id"},
		Predicates:  []columna.CostPredicate{intPredicate("id", columna.OpEq, 100)},
	})
	if pruned.Candidates[0].Cost != 0 {
		t.Fatalf("candidate-wide pruning must cost zero: %+v", pruned.Candidates[0])
	}
}

// ---- validation errors ------------------------------------------------------

func TestInvalidCandidates(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*columna.CandidateSource)
	}{
		{"negative rows", func(c *columna.CandidateSource) { c.Rows = -1 }},
		{"negative bytes", func(c *columna.CandidateSource) {
			c.Columns["id"] = cloneInput(c.Columns["id"], func(in *columna.ColumnInput) { in.Bytes[0] = -1 })
		}},
		{"nulls exceed rows", func(c *columna.CandidateSource) {
			in := c.Columns["id"]
			in.Nulls = 11
			c.Columns["id"] = in
		}},
		{"distinct exceeds non-null rows", func(c *columna.CandidateSource) {
			in := c.Columns["id"]
			in.Distinct = 11
			c.Columns["id"] = in
		}},
		{"min above max", func(c *columna.CandidateSource) {
			pages := c.Groups[0].Pages["id"]
			pages[0] = intStat(5, 0, 5, 9, 1)
		}},
		{"page nulls exceed page rows", func(c *columna.CandidateSource) {
			pages := c.Groups[0].Pages["id"]
			pages[0] = columna.PageStat{Rows: 5, Nulls: 6}
		}},
		{"page distinct exceeds non-null rows", func(c *columna.CandidateSource) {
			pages := c.Groups[0].Pages["id"]
			pages[0] = intStat(5, 0, 6, 1, 5)
		}},
		{"byte count disagrees with page count", func(c *columna.CandidateSource) {
			in := c.Columns["id"]
			in.Bytes = []int64{100}
			c.Columns["id"] = in
		}},
		{"missing read column metadata", func(c *columna.CandidateSource) {
			delete(c.Columns, "id")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := tenRowsCandidate("a")
			tc.mutate(&candidate)
			_, err := columna.EstimateScanCost(columna.ScanRequest{
				Sources:     []columna.CandidateSource{candidate},
				ReadColumns: []string{"id"},
			})
			var invalid *columna.InvalidArgumentError
			if !errors.As(err, &invalid) {
				t.Fatalf("want InvalidArgumentError, got %v", err)
			}
		})
	}
}

func TestEmptyCandidateList(t *testing.T) {
	_, err := columna.EstimateScanCost(columna.ScanRequest{ReadColumns: []string{"id"}})
	var invalid *columna.InvalidArgumentError
	if !errors.As(err, &invalid) {
		t.Fatalf("want InvalidArgumentError, got %v", err)
	}
}

func TestUnsupportedPredicates(t *testing.T) {
	v := columna.IntValue(1)
	cases := []struct {
		name string
		pred columna.CostPredicate
	}{
		{"unknown operator", columna.CostPredicate{Column: "id", Type: columna.TypeInt64, Op: "!=", Value: &v}},
		{"string ordering", columna.CostPredicate{Column: "s", Type: columna.TypeString, Op: columna.OpLt,
			Value: ptr(columna.StringValue("a"))}},
		{"unknown type", columna.CostPredicate{Column: "id", Type: "date", Op: columna.OpEq, Value: &v}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := tenRowsCandidate("a")
			candidate.Columns["s"] = columna.ColumnInput{Bytes: []int64{10, 10}, Distinct: 1, Set: true,
				Min: columna.StringValue("a"), Max: columna.StringValue("z")}
			candidate.Groups[0].Pages["s"] = []columna.PageStat{
				{Rows: 5, Distinct: 1, Set: true, Min: columna.StringValue("a"), Max: columna.StringValue("m")},
				{Rows: 5, Distinct: 1, Set: true, Min: columna.StringValue("n"), Max: columna.StringValue("z")},
			}
			_, err := columna.EstimateScanCost(columna.ScanRequest{
				Sources:     []columna.CandidateSource{candidate},
				ReadColumns: []string{"id", "s"},
				Predicates:  []columna.CostPredicate{tc.pred},
			})
			var unsupported *columna.UnsupportedPredicateError
			if !errors.As(err, &unsupported) {
				t.Fatalf("want UnsupportedPredicateError, got %v", err)
			}
		})
	}
}

// ---- footer adapter ----------------------------------------------------------

func TestFileMetadataCandidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.columna")
	if _, err := columna.WriteFile(path, eventSchema(), [][]columna.Row{eventRows()}, 3, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	metadata, err := columna.ReadFileMetadata(path)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	candidate, err := metadata.Candidate("events", []string{"id", "score"})
	if err != nil {
		t.Fatalf("candidate: %v", err)
	}
	if candidate.Rows != 12 || len(candidate.Groups) != 1 {
		t.Fatalf("candidate shape: rows=%d groups=%d", candidate.Rows, len(candidate.Groups))
	}
	if pages := len(candidate.Groups[0].Pages["id"]); pages != 4 {
		t.Fatalf("id pages = %d, want 4", pages)
	}
	if got := len(candidate.Columns["id"].Bytes); got != 4 {
		t.Fatalf("id byte counts = %d, want 4", got)
	}

	// A range estimate uses recorded min/max and is complete; an equality
	// estimate is incomplete because the footer stores no distinct count.
	rangeResult := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{candidate},
		ReadColumns: []string{"id"},
		Predicates:  []columna.CostPredicate{intPredicate("id", columna.OpGte, 5)},
	})
	if rangeResult.Candidates[0].IncompleteStatistics {
		t.Fatalf("range estimate over recorded min/max must be complete")
	}
	if rangeResult.Candidates[0].Bytes <= 0 {
		t.Fatalf("range estimate must count page bytes")
	}

	eqResult := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{candidate},
		ReadColumns: []string{"id"},
		Predicates:  []columna.CostPredicate{intPredicate("id", columna.OpEq, 7)},
	})
	if !eqResult.Candidates[0].IncompleteStatistics {
		t.Fatalf("equality estimate without a distinct count must be incomplete")
	}

	// The adapter never mutates the file and reads footer metadata only: the
	// file keeps its size and checksum.
	if info2, err := os.Stat(path); err != nil || info2.Size() != info.Size() {
		t.Fatalf("file changed during estimation: %v %v", err, info2)
	}
}

func TestTableCandidateAndPredicateConversion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.columna")
	table := writeEvents(t, path)
	candidate, err := columna.TableCandidate(table, []string{"id"})
	if err != nil {
		t.Fatalf("candidate: %v", err)
	}
	if candidate.Name != "events" || candidate.Rows != 12 {
		t.Fatalf("candidate = %+v", candidate)
	}
	bound := columna.Predicate{Column: "id", Op: columna.OpBetween,
		Lo: ptr(columna.IntValue(3)), Hi: ptr(columna.IntValue(9))}
	predicate, err := columna.CostPredicateFrom(bound, table.Schema)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{candidate},
		ReadColumns: []string{"id"},
		Predicates:  []columna.CostPredicate{predicate},
	})
	if result.Candidates[0].FilteredRows != 6 {
		t.Fatalf("between 3 and 9 filters ~6.25 of 12 rows, got %d", result.Candidates[0].FilteredRows)
	}
}

// ---- helpers ----------------------------------------------------------------

func TestMultiplePredicatesCombineByIndependence(t *testing.T) {
	candidate := columna.CandidateSource{
		Name: "two", Rows: 100,
		Groups: []columna.GroupStat{{
			Rows: 100,
			Pages: map[string][]columna.PageStat{
				"a": {intStat(100, 0, 10, 0, 9)},
				"b": {intStat(100, 0, 4, 0, 3)},
			},
		}},
		Columns: map[string]columna.ColumnInput{
			"a": {Bytes: []int64{100}, Distinct: 10, Set: true, Min: columna.IntValue(0), Max: columna.IntValue(9)},
			"b": {Bytes: []int64{100}, Distinct: 4, Set: true, Min: columna.IntValue(0), Max: columna.IntValue(3)},
		},
	}
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{candidate},
		ReadColumns: []string{"a", "b"},
		Predicates: []columna.CostPredicate{
			intPredicate("a", columna.OpEq, 5),
			intPredicate("b", columna.OpEq, 2),
		},
	})
	c := result.Candidates[0]
	// (1/10) * (1/4) * 100 = 2.5, rounded to three.
	if c.FilteredRows != 3 {
		t.Fatalf("filtered = %d, want 3", c.FilteredRows)
	}
	if c.Bytes != 200 || c.DecodedValues != 200 {
		t.Fatalf("both columns must be read: %+v", c)
	}
}

func TestLimitWithSelectivityStopsAcrossGroups(t *testing.T) {
	build := func(name string, bytes int64) columna.CandidateSource {
		c := columna.CandidateSource{Name: name, Rows: 20, Columns: map[string]columna.ColumnInput{}}
		for g := 0; g < 2; g++ {
			group := columna.GroupStat{Rows: 10, Pages: map[string][]columna.PageStat{
				"id": {intStat(10, 0, 10, 1, 10)},
			}}
			c.Groups = append(c.Groups, group)
		}
		c.Columns["id"] = columna.ColumnInput{Bytes: []int64{bytes, bytes}, Distinct: 10,
			Set: true, Min: columna.IntValue(1), Max: columna.IntValue(10)}
		return c
	}
	candidate := build("a", 100)
	// Equality selectivity 1/10. To collect limit 1, ceil(1/0.1)=10 rows must be
	// examined: exactly group 0, so group 1 is never read.
	result := estimate(t, columna.ScanRequest{
		Sources:     []columna.CandidateSource{candidate},
		ReadColumns: []string{"id"},
		Predicates:  []columna.CostPredicate{intPredicate("id", columna.OpEq, 3)},
		Limit:       i64(1),
	})
	c := result.Candidates[0]
	if c.RowGroups != 1 || c.Bytes != 100 || c.ProcessedRows != 10 || c.ReturnedRows != 1 {
		t.Fatalf("selective early stop: %+v", c)
	}
}

func ptr(v columna.Value) *columna.Value { return &v }

func cloneInput(in columna.ColumnInput, adjust func(*columna.ColumnInput)) columna.ColumnInput {
	if in.Bytes != nil {
		in.Bytes = append([]int64{}, in.Bytes...)
	}
	adjust(&in)
	return in
}

func deepEqual(a, b columna.ScanCostResult) bool {
	if a.Selected != b.Selected || len(a.Candidates) != len(b.Candidates) {
		return false
	}
	for i := range a.Candidates {
		x, y := a.Candidates[i], b.Candidates[i]
		if x != y {
			return false
		}
	}
	return true
}
