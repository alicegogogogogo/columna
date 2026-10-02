// Command columna is the CLI and HTTP server of the Columna columnar format.
//
//	columna serve   --data <dir> --host 127.0.0.1 --port 18088
//	columna create  --file <path> --schema 'id:int64,name:string?'
//	columna write   --file <path> --schema 'id:int64,name:string?' --rows rows.json
//	columna query   --file <path> --select name --where 'id > 2' --agg 'sum:score'
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"columna/internal/columna"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "create":
		err = runCreate(os.Args[2:])
	case "write":
		err = runWrite(os.Args[2:])
	case "query":
		err = runQuery(os.Args[2:])
	case "stats":
		err = runStats(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "columna: %s\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Columna columnar files

  columna serve  --data <dir> [--host 127.0.0.1] [--port 18080]
  columna create --file <path> --schema 'name:type[?],...'
  columna write  --file <path> --schema 'name:type[?],...' --rows <jsonl|json> [--append]
  columna query  --file <path> [--select a,b] [--where 'a > 1'] [--agg 'sum:b'] [--limit n]
  columna stats  --file <path>
`)
}

func runServe(args []string) error {
	set := flag.NewFlagSet("serve", flag.ContinueOnError)
	data := set.String("data", ".", "data directory holding .columna files")
	host := set.String("host", "127.0.0.1", "listen host")
	port := set.Int("port", 18080, "listen port")
	if err := set.Parse(args); err != nil {
		return err
	}
	service, err := columna.OpenService(*data)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		return fmt.Errorf("cannot listen on %s:%d: %v", *host, *port, err)
	}
	server := &http.Server{Handler: &columna.Handler{Service: service}}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		server.Close()
	}()
	fmt.Printf("Columna listening on http://%s:%d\n", *host, *port)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func parseSchema(spec string) (columna.Schema, error) {
	schema := columna.Schema{}
	for _, field := range strings.Split(spec, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		parts := strings.SplitN(field, ":", 2)
		if len(parts) != 2 {
			return schema, fmt.Errorf("schema field %q must look like name:type", field)
		}
		column := columna.Column{Name: strings.TrimSpace(parts[0]), Type: strings.TrimSpace(parts[1])}
		if strings.HasSuffix(column.Type, "?") {
			column.Nullable = true
			column.Type = strings.TrimSuffix(column.Type, "?")
		}
		schema.Columns = append(schema.Columns, column)
	}
	if err := schema.Validate(); err != nil {
		return schema, err
	}
	return schema, nil
}

func runCreate(args []string) error {
	set := flag.NewFlagSet("create", flag.ContinueOnError)
	file := set.String("file", "", "table file path")
	spec := set.String("schema", "", "schema, e.g. 'id:int64,name:string?'")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *file == "" || *spec == "" {
		return fmt.Errorf("create requires --file and --schema")
	}
	schema, err := parseSchema(*spec)
	if err != nil {
		return err
	}
	if _, err := os.Stat(*file); err == nil {
		return fmt.Errorf("file %s already exists", *file)
	}
	if _, err := columna.WriteFile(*file, schema, nil, columna.DefaultMaxPage, nil); err != nil {
		return err
	}
	fmt.Printf("created %s with %d columns\n", *file, len(schema.Columns))
	return nil
}

func runWrite(args []string) error {
	set := flag.NewFlagSet("write", flag.ContinueOnError)
	file := set.String("file", "", "table file path")
	spec := set.String("schema", "", "schema used when the file is created")
	rowsPath := set.String("rows", "", "JSONL or JSON array file")
	appendMode := set.Bool("append", false, "append a row group to an existing file")
	maxPage := set.Int("max-page-values", columna.DefaultMaxPage, "values per page")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *file == "" || *rowsPath == "" {
		return fmt.Errorf("write requires --file and --rows")
	}
	raw, err := os.ReadFile(*rowsPath)
	if err != nil {
		return err
	}
	var schema columna.Schema
	if *appendMode {
		table, err := columna.OpenPath(*file)
		if err != nil {
			return err
		}
		schema = table.Schema
	} else {
		if *spec == "" {
			return fmt.Errorf("write requires --schema when creating a new file")
		}
		schema, err = parseSchema(*spec)
		if err != nil {
			return err
		}
	}
	rows, err := columna.DecodeLines(schema, raw)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("no rows found in %s", *rowsPath)
	}
	if *appendMode {
		parsed, err := columna.AppendRows(*file, schema, rows, *maxPage, nil)
		if err != nil {
			return err
		}
		stats := parsed.Stats()
		fmt.Printf("appended %d rows to %s (%d rows, %d row groups)\n", len(rows), *file, stats.RowCount, stats.RowGroupCount)
		return nil
	}
	parsed, err := columna.WriteFile(*file, schema, [][]columna.Row{rows}, *maxPage, nil)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %d rows to %s (%d row groups)\n", len(rows), *file, parsed.Stats().RowGroupCount)
	return nil
}

func runQuery(args []string) error {
	set := flag.NewFlagSet("query", flag.ContinueOnError)
	file := set.String("file", "", "table file path")
	selectSpec := set.String("select", "", "comma separated projection")
	whereSpec := multiFlag{}
	set.Var(&whereSpec, "where", "predicate, e.g. 'age >= 30' or 'age between 1 9'")
	aggSpec := multiFlag{}
	set.Var(&aggSpec, "agg", "aggregate, e.g. 'sum:amount' or 'count'")
	limit := set.Int("limit", -1, "maximum rows")
	noPushdown := set.Bool("no-pushdown", false, "disable page statistics pruning")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("query requires --file")
	}
	table, err := columna.OpenPath(*file)
	if err != nil {
		return err
	}
	query := columna.Query{Table: table.Name}
	if *selectSpec != "" {
		for _, name := range strings.Split(*selectSpec, ",") {
			if name = strings.TrimSpace(name); name != "" {
				query.Projection = append(query.Projection, name)
			}
		}
	}
	for _, predicate := range whereSpec {
		parsed, err := parsePredicate(predicate)
		if err != nil {
			return err
		}
		query.Predicates = append(query.Predicates, parsed)
	}
	for _, aggregate := range aggSpec {
		parts := strings.SplitN(strings.TrimSpace(aggregate), ":", 2)
		entry := columna.Aggregate{Func: parts[0]}
		if len(parts) == 2 {
			entry.Column = strings.TrimSpace(parts[1])
		}
		query.Aggregates = append(query.Aggregates, entry)
	}
	if *limit >= 0 {
		query.Limit = limit
	}
	pushdown := !*noPushdown
	query.Pushdown = &pushdown
	result, err := columna.Execute(table, query)
	if err != nil {
		return err
	}
	out, err := json.Marshal(result)
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

func parsePredicate(text string) (columna.Predicate, error) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return columna.Predicate{}, fmt.Errorf("empty predicate")
	}
	predicate := columna.Predicate{Column: fields[0]}
	switch {
	case len(fields) == 3 && fields[1] == "between":
		return predicate, fmt.Errorf("between needs a lower and an upper bound")
	case len(fields) == 4 && fields[1] == "between":
		lo, err := parseLiteral(fields[2])
		if err != nil {
			return predicate, err
		}
		hi, err := parseLiteral(fields[3])
		if err != nil {
			return predicate, err
		}
		predicate.Op = columna.OpBetween
		predicate.Lo, predicate.Hi = lo, hi
		return predicate, nil
	case len(fields) == 3:
		value, err := parseLiteral(fields[2])
		if err != nil {
			return predicate, err
		}
		predicate.Op = fields[1]
		predicate.Value = value
		return predicate, nil
	}
	return predicate, fmt.Errorf("cannot parse predicate %q", text)
}

func parseLiteral(text string) (*columna.Value, error) {
	if text == "true" || text == "false" {
		value := columna.BoolValue(text == "true")
		return &value, nil
	}
	if number, err := strconv.ParseInt(text, 10, 64); err == nil {
		value := columna.IntValue(number)
		return &value, nil
	}
	if number, err := strconv.ParseFloat(text, 64); err == nil {
		value := columna.FloatValue(number)
		return &value, nil
	}
	value := columna.StringValue(strings.Trim(text, `"'`))
	return &value, nil
}

func runStats(args []string) error {
	set := flag.NewFlagSet("stats", flag.ContinueOnError)
	file := set.String("file", "", "table file path")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("stats requires --file")
	}
	table, err := columna.OpenPath(*file)
	if err != nil {
		return err
	}
	checksum, err := table.Checksum()
	if err != nil {
		return err
	}
	out, err := json.Marshal(columna.StatsResult{
		Table:    table.Name,
		Schema:   table.Schema,
		Stats:    table.Stats(),
		Checksum: checksum,
	})
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}
