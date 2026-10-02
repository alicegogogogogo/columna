package columna

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Handler serves the Columna HTTP API documented in README.md.
type Handler struct {
	Service *Service
}

// ServeHTTP dispatches one request and writes the JSON response.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status, body, err := h.dispatch(r)
	if err != nil {
		status, body = ErrorResponse(err)
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		raw = []byte(`{"error":{"code":"internal_error","message":"response could not be encoded"}}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(status)
	w.Write(raw)
}

func (h *Handler) dispatch(r *http.Request) (int, any, error) {
	parts := splitPath(r.URL.Path)
	switch {
	case r.Method == http.MethodGet && len(parts) == 1 && parts[0] == "health":
		return 200, map[string]any{"status": "ok"}, nil
	case r.Method == http.MethodPost && len(parts) == 1 && parts[0] == "tables":
		return h.createTable(r)
	case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "tables" && parts[2] == "rows":
		return h.appendRows(r, parts[1])
	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "tables" && parts[2] == "stats":
		result, err := h.Service.Stats(parts[1])
		if err != nil {
			return 0, nil, err
		}
		return 200, result, nil
	case r.Method == http.MethodPost && len(parts) == 1 && parts[0] == "query":
		return h.query(r)
	}
	return 0, nil, errorf(CodeNotFound, "route was not found")
}

func (h *Handler) createTable(r *http.Request) (int, any, error) {
	raw, err := readBody(r)
	if err != nil {
		return 0, nil, err
	}
	var request struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
	}
	if err := unmarshalStrict(raw, &request); err != nil {
		return 0, nil, Validationf("invalid create request: %v", err)
	}
	if request.Name == "" {
		return 0, nil, Validationf("name is required")
	}
	if len(request.Schema) == 0 {
		return 0, nil, Validationf("schema is required")
	}
	schema, err := schemaFromRequest(request.Schema)
	if err != nil {
		return 0, nil, err
	}
	result, err := h.Service.CreateTable(request.Name, schema, r.Header.Get("Idempotency-Key"))
	if err != nil {
		return 0, nil, err
	}
	return 201, result, nil
}

func (h *Handler) appendRows(r *http.Request, name string) (int, any, error) {
	raw, err := readBody(r)
	if err != nil {
		return 0, nil, err
	}
	var request AppendRequest
	if err := unmarshalStrict(raw, &request); err != nil {
		return 0, nil, Validationf("invalid append request: %v", err)
	}
	if len(request.Rows) == 0 {
		return 0, nil, Validationf("rows is required")
	}
	result, err := h.Service.AppendRows(name, request, r.Header.Get("Idempotency-Key"))
	if err != nil {
		return 0, nil, err
	}
	return 201, result, nil
}

func (h *Handler) query(r *http.Request) (int, any, error) {
	raw, err := readBody(r)
	if err != nil {
		return 0, nil, err
	}
	query, err := DecodeQuery(raw)
	if err != nil {
		return 0, nil, err
	}
	result, err := h.Service.Query(query)
	if err != nil {
		return 0, nil, err
	}
	return 200, result, nil
}

func readBody(r *http.Request) (json.RawMessage, error) {
	if r.Body == nil {
		return nil, Validationf("a JSON body is required")
	}
	contentType := r.Header.Get("Content-Type")
	if base := strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]); base != "application/json" {
		return nil, Validationf("Content-Type must be application/json")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, Validationf("request body could not be read")
	}
	if len(raw) == 0 {
		return nil, Validationf("a JSON body is required")
	}
	return json.RawMessage(raw), nil
}

func splitPath(path string) []string {
	parts := []string{}
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

// ErrorResponse renders the shared error envelope and its HTTP status.
func ErrorResponse(err error) (int, map[string]any) {
	var target *Error
	if !errors.As(err, &target) {
		return 500, map[string]any{"error": map[string]any{"code": CodeInternal, "message": "internal server error"}}
	}
	return Status(err), map[string]any{"error": map[string]any{"code": target.Code, "message": target.Message}}
}
