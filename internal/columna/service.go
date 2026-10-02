package columna

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Service owns every table in one data directory. Every mutating operation
// takes a single lock, so the service behaves as one serializable writer.
type Service struct {
	dir       string
	mu        sync.Mutex
	tables    map[string]*Table
	completed map[string]string
	sidecar   string
}

// MaxRowsPerGroup bounds one POST /tables/{name}/rows request.
const MaxRowsPerGroup = 50000

// OpenService loads (or prepares) the data directory.
func OpenService(dir string) (*Service, error) {
	if dir == "" {
		return nil, Validationf("data directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errorf(CodeInternal, "cannot create %s: %v", dir, err)
	}
	service := &Service{
		dir:       dir,
		tables:    map[string]*Table{},
		completed: map[string]string{},
		sidecar:   filepath.Join(dir, ".idempotency.json"),
	}
	names, err := ListTables(dir)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		table, err := OpenTable(dir, name)
		if err != nil {
			return nil, err
		}
		service.tables[name] = table
	}
	if raw, err := os.ReadFile(service.sidecar); err == nil {
		var stored map[string]string
		if err := json.Unmarshal(raw, &stored); err == nil {
			service.completed = stored
		}
	}
	return service, nil
}

// Dir returns the data directory of the service.
func (s *Service) Dir() string { return s.dir }

// CreateTable implements POST /tables.
func (s *Service) CreateTable(name string, schema Schema, key string) (TableResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status, raw, err := s.idempotent("POST /tables", key, map[string]any{"name": name, "schema": schema}, func() (int, any, error) {
		if _, exists := s.tables[name]; exists {
			return 0, nil, errorf(CodeConflict, "table %q already exists", name)
		}
		table, err := CreateTable(s.dir, name, schema)
		if err != nil {
			return 0, nil, err
		}
		s.tables[name] = table
		return 201, tableResult(table), nil
	})
	if err != nil {
		return TableResult{}, err
	}
	if status != 201 {
		return TableResult{}, errorf(CodeInternal, "unexpected status %d", status)
	}
	var result TableResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return TableResult{}, err
		}
	}
	return result, nil
}

// AppendRows implements POST /tables/{name}/rows.
func (s *Service) AppendRows(name string, body AppendRequest, key string) (AppendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	table, ok := s.tables[name]
	if !ok {
		return AppendResult{}, errorf(CodeNotFound, "table %q does not exist", name)
	}
	rows, err := DecodeRows(table.Schema, body.Rows)
	if err != nil {
		return AppendResult{}, err
	}
	if len(rows) == 0 {
		return AppendResult{}, Validationf("rows must contain at least one object")
	}
	if len(rows) > MaxRowsPerGroup {
		return AppendResult{}, Validationf("a row group holds at most %d rows", MaxRowsPerGroup)
	}
	maxPerPage := DefaultMaxPage
	if body.MaxPageValues != nil {
		if *body.MaxPageValues <= 0 {
			return AppendResult{}, Validationf("max_page_values must be positive")
		}
		maxPerPage = *body.MaxPageValues
	}
	forced := map[string]string{}
	for columnName, encoding := range body.Encoding {
		if _, ok := table.Schema.Column(columnName); !ok {
			return AppendResult{}, Validationf("unknown column %q in encoding", columnName)
		}
		switch encoding {
		case EncodingPlain, EncodingDictionary, EncodingRLE:
		default:
			return AppendResult{}, Validationf("unsupported encoding %q for column %q", encoding, columnName)
		}
		forced[columnName] = encoding
	}
	status, raw, err := s.idempotent("POST /tables/"+name+"/rows", key, map[string]any{"name": name, "body": body}, func() (int, any, error) {
		if err := table.Append(rows, maxPerPage, forced); err != nil {
			return 0, nil, err
		}
		return 201, appendResult(table, len(rows)), nil
	})
	if err != nil {
		return AppendResult{}, err
	}
	if status != 201 {
		return AppendResult{}, errorf(CodeInternal, "unexpected status %d", status)
	}
	var result AppendResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return AppendResult{}, err
		}
	}
	return result, nil
}

// Table looks up a table by name.
func (s *Service) Table(name string) (*Table, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	table, ok := s.tables[name]
	if !ok {
		return nil, errorf(CodeNotFound, "table %q does not exist", name)
	}
	return table, nil
}

// Stats implements GET /tables/{name}/stats.
func (s *Service) Stats(name string) (StatsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	table, ok := s.tables[name]
	if !ok {
		return StatsResult{}, errorf(CodeNotFound, "table %q does not exist", name)
	}
	checksum, err := table.Checksum()
	if err != nil {
		return StatsResult{}, err
	}
	return StatsResult{
		Table:    name,
		Schema:   table.Schema,
		Stats:    table.Stats(),
		Checksum: checksum,
	}, nil
}

// Query implements POST /query.
func (s *Service) Query(query Query) (QueryResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	table, ok := s.tables[query.Table]
	if !ok {
		return QueryResult{}, errorf(CodeNotFound, "table %q does not exist", query.Table)
	}
	return Execute(table, query)
}

// TableNames lists the loaded tables in sorted order.
func (s *Service) TableNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.tables))
	for name := range s.tables {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// idempotent runs operation once per key. Replaying a key returns the recorded
// status and response body; reusing it for a different operation is a
// conflict. Because a read-modify-write POST has exactly one request shape for
// a given operation, the body itself is not part of the key.
func (s *Service) idempotent(operation, key string, request any, run func() (int, any, error)) (int, json.RawMessage, error) {
	if key == "" {
		status, response, err := run()
		if err != nil {
			return 0, nil, err
		}
		raw, _ := json.Marshal(response)
		return status, raw, nil
	}
	record := canonical(operation, request)
	if existing, ok := s.completed[key]; ok {
		var stored struct {
			Request  string          `json:"request"`
			Status   int             `json:"status"`
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal([]byte(existing), &stored); err == nil && stored.Request != "" {
			if stored.Request != record {
				return 0, nil, errorf(CodeIdempotency, "idempotency key %q was already used for another operation", key)
			}
			return stored.Status, stored.Response, nil
		}
	}
	status, response, err := run()
	if err != nil {
		return 0, nil, err
	}
	body, err := json.Marshal(response)
	if err != nil {
		return 0, nil, err
	}
	entry, err := json.Marshal(map[string]any{
		"request":  record,
		"status":   status,
		"response": json.RawMessage(body),
	})
	if err != nil {
		return 0, nil, err
	}
	s.completed[key] = string(entry)
	if err := s.persistIdempotency(); err != nil {
		return 0, nil, err
	}
	return status, body, nil
}

func (s *Service) persistIdempotency() error {
	raw, err := json.Marshal(s.completed)
	if err != nil {
		return errorf(CodeInternal, "cannot encode the idempotency store: %v", err)
	}
	temp := s.sidecar + ".tmp"
	if err := os.WriteFile(temp, raw, 0o644); err != nil {
		return errorf(CodeInternal, "cannot write %s: %v", temp, err)
	}
	if err := os.Rename(temp, s.sidecar); err != nil {
		return errorf(CodeInternal, "cannot replace %s: %v", s.sidecar, err)
	}
	return nil
}

func canonical(operation string, request any) string {
	raw, err := json.Marshal(map[string]any{"operation": operation, "request": request})
	if err != nil {
		return operation
	}
	return string(raw)
}

// TableResult is the body of POST /tables.
type TableResult struct {
	Name      string `json:"name"`
	Schema    Schema `json:"schema"`
	RowCount  int64  `json:"row_count"`
	RowGroups int    `json:"row_groups"`
}

func tableResult(table *Table) TableResult {
	stats := table.Stats()
	return TableResult{
		Name:      table.Name,
		Schema:    table.Schema,
		RowCount:  stats.RowCount,
		RowGroups: stats.RowGroupCount,
	}
}

// AppendResult is the body of POST /tables/{name}/rows.
type AppendResult struct {
	Table         string        `json:"table"`
	AppendedRows  int           `json:"appended_rows"`
	RowCount      int64         `json:"row_count"`
	RowGroupCount int           `json:"row_group_count"`
	RowGroupIndex int           `json:"row_group_index"`
	Encodings     []EncodingUse `json:"encodings"`
	FileSize      int64         `json:"file_size"`
}

// EncodingUse reports how many pages of one column used each encoding.
type EncodingUse struct {
	Column string         `json:"column"`
	Pages  map[string]int `json:"pages"`
}

func appendResult(table *Table, appended int) AppendResult {
	stats := table.Stats()
	return AppendResult{
		Table:         table.Name,
		AppendedRows:  appended,
		RowCount:      stats.RowCount,
		RowGroupCount: stats.RowGroupCount,
		RowGroupIndex: stats.RowGroupCount - 1,
		Encodings:     encodingUse(table),
		FileSize:      stats.FileSize,
	}
}

func encodingUse(table *Table) []EncodingUse {
	groups := table.parsed.rowGroups
	if len(groups) == 0 {
		return []EncodingUse{}
	}
	group := groups[len(groups)-1]
	out := make([]EncodingUse, 0, len(table.Schema.Columns))
	for _, column := range table.Schema.Columns {
		pages := map[string]int{
			EncodingPlain:      0,
			EncodingDictionary: 0,
			EncodingRLE:        0,
		}
		for _, header := range group.Pages[column.Name] {
			pages[header.Encoding]++
		}
		out = append(out, EncodingUse{Column: column.Name, Pages: pages})
	}
	return out
}

// StatsResult is the body of GET /tables/{name}/stats.
type StatsResult struct {
	Table    string    `json:"table"`
	Schema   Schema    `json:"schema"`
	Stats    FileStats `json:"stats"`
	Checksum string    `json:"checksum"`
}

// AppendRequest is the body of POST /tables/{name}/rows.
type AppendRequest struct {
	Rows          json.RawMessage   `json:"rows"`
	MaxPageValues *int              `json:"max_page_values"`
	Encoding      map[string]string `json:"encoding"`
}
