package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"columna/internal/columna"
)

// eventSchema is the schema used by the query engine tests.
func eventSchema() columna.Schema {
	return columna.Schema{Columns: []columna.Column{
		{Name: "id", Type: columna.TypeInt64},
		{Name: "region", Type: columna.TypeString},
		{Name: "score", Type: columna.TypeFloat64, Nullable: true},
		{Name: "flag", Type: columna.TypeBool},
	}}
}

// eventRows builds 12 rows in four pages of three values, with null scores.
func eventRows() []columna.Row {
	rows := make([]columna.Row, 0, 12)
	for i := 1; i <= 12; i++ {
		score := columna.FloatValue(float64(i) / 2)
		if i%4 == 0 {
			score = columna.NullValue()
		}
		region := "north"
		if i%2 == 0 {
			region = "south"
		}
		rows = append(rows, columna.Row{
			columna.IntValue(int64(i)),
			columna.StringValue(region),
			score,
			columna.BoolValue(i%3 == 0),
		})
	}
	return rows
}

func writeEvents(t *testing.T, path string) *columna.Table {
	t.Helper()
	if _, err := columna.WriteFile(path, eventSchema(), [][]columna.Row{eventRows()}, 3, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	table, err := columna.OpenPath(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return table
}

func rowsJSON(t *testing.T, result columna.QueryResult) string {
	t.Helper()
	raw, err := json.Marshal(result.Rows)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestPushdownNeverChangesTheResultSet runs every supported operator with page
// statistics pruning on and off and requires identical rows. Page statistics
// may only skip a page when no row in it can match, so the two modes must agree.
func TestPushdownNeverChangesTheResultSet(t *testing.T) {
	table := writeEvents(t, filepath.Join(t.TempDir(), "events.columna"))
	bodies := []string{
		`{"table":"events","predicates":[{"column":"id","op":">=","value":5}]}`,
		`{"table":"events","predicates":[{"column":"id","op":"<","value":4}]}`,
		`{"table":"events","predicates":[{"column":"id","op":"<=","value":4}]}`,
		`{"table":"events","predicates":[{"column":"id","op":">","value":9}]}`,
		`{"table":"events","predicates":[{"column":"id","op":"=","value":7}]}`,
		`{"table":"events","predicates":[{"column":"id","op":"between","lo":3,"hi":9}]}`,
		`{"table":"events","predicates":[{"column":"score","op":">","value":2.5}]}`,
		`{"table":"events","predicates":[{"column":"score","op":"between","lo":2,"hi":5}]}`,
		`{"table":"events","predicates":[{"column":"region","op":"=","value":"north"}]}`,
		`{"table":"events","predicates":[{"column":"flag","op":"=","value":true}]}`,
		`{"table":"events","predicates":[{"column":"id","op":">=","value":2},{"column":"region","op":"=","value":"south"}]}`,
	}
	pruned := 0
	for _, body := range bodies {
		query, err := columna.DecodeQuery(json.RawMessage(body))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		query.Projection = []string{"id", "region", "score", "flag"}
		withPushdown := true
		query.Pushdown = &withPushdown
		prunedResult, err := columna.Execute(table, query)
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		noPushdown := false
		query.Pushdown = &noPushdown
		scannedResult, err := columna.Execute(table, query)
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if rowsJSON(t, prunedResult) != rowsJSON(t, scannedResult) {
			t.Fatalf("%s: pushdown returned %s, full scan returned %s",
				body, rowsJSON(t, prunedResult), rowsJSON(t, scannedResult))
		}
		if scannedResult.SkippedPages != 0 {
			t.Fatalf("%s: a full scan skipped %d pages", body, scannedResult.SkippedPages)
		}
		if prunedResult.ScannedRows > scannedResult.ScannedRows {
			t.Fatalf("%s: pushdown examined %d rows, full scan %d", body, prunedResult.ScannedRows, scannedResult.ScannedRows)
		}
		pruned += prunedResult.SkippedPages
	}
	if pruned == 0 {
		t.Fatal("no predicate pruned a page, so the pushdown path was never exercised")
	}
}

func TestProjectionLimitAndAggregates(t *testing.T) {
	table := writeEvents(t, filepath.Join(t.TempDir(), "events.columna"))

	projection := columna.Query{Table: "events", Projection: []string{"region", "id"}}
	result, err := columna.Execute(table, projection)
	if err != nil {
		t.Fatal(err)
	}
	if result.RowCount != 12 || len(result.Rows[0]) != 2 {
		t.Fatalf("projection returned %d rows of %d columns", result.RowCount, len(result.Rows[0]))
	}
	if _, ok := result.Rows[0]["region"]; !ok {
		t.Fatalf("projection dropped the region column: %v", result.Rows[0])
	}

	limit := 3
	limited := columna.Query{Table: "events", Projection: []string{"id"}, Limit: &limit}
	result, err = columna.Execute(table, limited)
	if err != nil {
		t.Fatal(err)
	}
	if result.RowCount != 3 || result.Rows[0]["id"] != int64(1) {
		t.Fatalf("limit 3 returned %+v", result.Rows)
	}
	zero := 0
	limited.Limit = &zero
	result, err = columna.Execute(table, limited)
	if err != nil {
		t.Fatal(err)
	}
	if result.RowCount != 0 {
		t.Fatalf("limit 0 returned %d rows", result.RowCount)
	}

	aggregates := columna.Query{Table: "events", Aggregates: []columna.Aggregate{
		{Func: columna.AggCount},
		{Func: columna.AggCount, Column: "score"},
		{Func: columna.AggSum, Column: "score"},
		{Func: columna.AggMin, Column: "region"},
		{Func: columna.AggMax, Column: "id"},
	}}
	result, err = columna.Execute(table, aggregates)
	if err != nil {
		t.Fatal(err)
	}
	if result.RowCount != 1 {
		t.Fatalf("aggregates returned %d rows", result.RowCount)
	}
	want := map[string]any{
		"count":       int64(12),
		"count_score": int64(9),
		"sum_score":   27.0, // 1..12 halves minus the three NULL scores
		"min_region":  "north",
		"max_id":      int64(12),
	}
	for name, expected := range want {
		if got := result.Rows[0][name]; got != expected {
			t.Fatalf("aggregate %s is %v, want %v", name, got, expected)
		}
	}
	if _, err := columna.Execute(table, columna.Query{Table: "events", Aggregates: []columna.Aggregate{
		{Func: columna.AggSum, Column: "region"},
	}}); err == nil {
		t.Fatal("sum over a string column must be rejected")
	}
}

// TestAppendReproducesASingleWrite requires the append path to rebuild the file
// byte for byte: an append must lay out the existing row groups exactly as the
// original writer did and continue the data area after them.
func TestAppendReproducesASingleWrite(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first.columna")
	second := filepath.Join(directory, "second.columna")
	rows := eventRows()
	head, tail := rows[:4], rows[4:]

	if _, err := columna.WriteFile(first, eventSchema(), [][]columna.Row{head}, 3, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := columna.AppendRows(first, eventSchema(), tail, 3, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := columna.WriteFile(second, eventSchema(), [][]columna.Row{head, tail}, 3, nil); err != nil {
		t.Fatal(err)
	}
	appended, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(appended) != string(written) {
		t.Fatalf("append produced %d bytes, a single write produced %d", len(appended), len(written))
	}
	file, err := columna.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	values, err := file.ColumnValues("id")
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		if values[i] != rows[i][0] {
			t.Fatalf("row %d is %+v after the append", i, values[i])
		}
	}
}

// TestAllNullPageStatisticsRoundTrip covers a page whose every value is NULL:
// its boundary statistics must be recorded as absent, and the column summary
// must still report the right null count.
func TestAllNullPageStatisticsRoundTrip(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "nulls.columna")
	schema := columna.Schema{Columns: []columna.Column{
		{Name: "id", Type: columna.TypeInt64},
		{Name: "note", Type: columna.TypeString, Nullable: true},
	}}
	rows := []columna.Row{
		{columna.IntValue(1), columna.NullValue()},
		{columna.IntValue(2), columna.NullValue()},
		{columna.IntValue(3), columna.StringValue("later")},
	}
	file, err := columna.WriteFile(path, schema, [][]columna.Row{rows}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	header := file.RowGroups()[0].Pages["note"][0]
	if header.ValueCount != 2 {
		t.Fatalf("first page holds %d values", header.ValueCount)
	}
	stats := columna.StatsOfPage(header)
	if stats.Set || stats.Nulls != 2 {
		t.Fatalf("all-null page statistics are %+v", stats)
	}
	for _, column := range file.Stats().Columns {
		if column.Name != "note" {
			continue
		}
		if column.NullCount != 2 || column.RowCount != 3 {
			t.Fatalf("column summary is %+v", column)
		}
		// Only the second page holds a value, so it defines both boundaries.
		if column.Min != columna.StringValue("later") || column.Max != columna.StringValue("later") {
			t.Fatalf("column summary recorded boundaries %v/%v", column.Min, column.Max)
		}
	}
	table, err := columna.OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	query := columna.Query{Table: "nulls", Predicates: []columna.Predicate{
		{Column: "note", Op: columna.OpEq, Value: valueOf(columna.StringValue("later"))},
	}}
	withPushdown := true
	query.Pushdown = &withPushdown
	pruned, err := columna.Execute(table, query)
	if err != nil {
		t.Fatal(err)
	}
	noPushdown := false
	query.Pushdown = &noPushdown
	scanned, err := columna.Execute(table, query)
	if err != nil {
		t.Fatal(err)
	}
	if rowsJSON(t, pruned) != rowsJSON(t, scanned) {
		t.Fatalf("null page: pushdown %s, full scan %s", rowsJSON(t, pruned), rowsJSON(t, scanned))
	}
	if pruned.RowCount != 1 || pruned.SkippedPages != 1 {
		t.Fatalf("predicate over an all-null page returned %d rows and skipped %d pages", pruned.RowCount, pruned.SkippedPages)
	}
}

func valueOf(v columna.Value) *columna.Value { return &v }
