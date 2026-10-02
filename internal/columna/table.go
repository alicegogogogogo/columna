package columna

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Table is one Columna file plus its loaded metadata.
type Table struct {
	Name   string
	Path   string
	Schema Schema

	parsed *File
}

// DataDir returns the directory that holds a table file.
func (t *Table) DataDir() string { return filepath.Dir(t.Path) }

// Stats returns the footer statistics of the table.
func (t *Table) Stats() FileStats { return t.parsed.stats }

// Checksum returns the hex SHA-256 recorded in the file trailer.
func (t *Table) Checksum() (string, error) { return Checksum(t.Path) }

// FileName maps a table name onto its file inside dir.
func FileName(dir, name string) string { return filepath.Join(dir, name+".columna") }

func validateTableName(name string) error {
	if !ValidName(name) || name == "" {
		return Validationf("invalid table name %q: use letters, digits and underscores, starting with a letter or underscore", name)
	}
	if strings.HasPrefix(name, ".") {
		return Validationf("invalid table name %q", name)
	}
	return nil
}

// CreateTable writes an empty Columna file for a new table.
func CreateTable(dir, name string, schema Schema) (*Table, error) {
	if err := validateTableName(name); err != nil {
		return nil, err
	}
	if err := schema.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errorf(CodeInternal, "cannot create %s: %v", dir, err)
	}
	path := FileName(dir, name)
	if _, err := os.Stat(path); err == nil {
		return nil, errorf(CodeConflict, "table %q already exists", name)
	}
	parsed, err := WriteFile(path, schema, nil, DefaultMaxPage, nil)
	if err != nil {
		return nil, err
	}
	return &Table{Name: name, Path: path, Schema: schema, parsed: parsed}, nil
}

// OpenTable loads an existing table file.
func OpenTable(dir, name string) (*Table, error) {
	if err := validateTableName(name); err != nil {
		return nil, err
	}
	path := FileName(dir, name)
	parsed, err := ReadFile(path)
	if err != nil {
		if _, ok := err.(*Error); ok && CodeOf(err) == CodeNotFound {
			return nil, errorf(CodeNotFound, "table %q does not exist", name)
		}
		return nil, err
	}
	return &Table{Name: name, Path: path, Schema: parsed.schema, parsed: parsed}, nil
}

// OpenPath loads a table file from an explicit path (used by the CLI).
func OpenPath(path string) (*Table, error) {
	parsed, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSuffix(filepath.Base(path), ".columna")
	return &Table{Name: name, Path: path, Schema: parsed.schema, parsed: parsed}, nil
}

// Reload refreshes the in-memory view after the file changed on disk.
func (t *Table) Reload() error {
	parsed, err := ReadFile(t.Path)
	if err != nil {
		return err
	}
	t.parsed = parsed
	t.Schema = parsed.schema
	return nil
}

// Append writes one row group. Rows must already match the schema.
func (t *Table) Append(rows []Row, maxPerPage int, forced map[string]string) error {
	if len(rows) == 0 {
		return Validationf("rows must not be empty")
	}
	parsed, err := AppendRows(t.Path, t.Schema, rows, maxPerPage, forced)
	if err != nil {
		return err
	}
	t.parsed = parsed
	return nil
}

// ListTables returns the sorted table names in dir.
func ListTables(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, errorf(CodeInternal, "cannot list %s: %v", dir, err)
	}
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".columna") {
			continue
		}
		names = append(names, strings.TrimSuffix(entry.Name(), ".columna"))
	}
	sort.Strings(names)
	return names, nil
}

// schemaFromRequest decodes the schema part of POST /tables.
func schemaFromRequest(raw json.RawMessage) (Schema, error) {
	var schema Schema
	if err := unmarshalStrict(raw, &schema); err != nil {
		return Schema{}, Validationf("invalid schema: %v", err)
	}
	if err := schema.Validate(); err != nil {
		return Schema{}, err
	}
	return schema, nil
}
