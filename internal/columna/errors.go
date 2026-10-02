package columna

import "fmt"

// Error codes used across the CLI and the HTTP service.
const (
	CodeValidation  = "validation_error"
	CodeNotFound    = "not_found"
	CodeConflict    = "conflict"
	CodeFormat      = "format_error"
	CodeInternal    = "internal_error"
	CodeIdempotency = "idempotency_conflict"
)

// Error is the single error type produced by this package. Code is a
// snake_case identifier that the HTTP layer maps onto a status code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func Validationf(format string, args ...any) *Error {
	return errorf(CodeValidation, format, args...)
}

func Formatf(format string, args ...any) *Error {
	return errorf(CodeFormat, format, args...)
}

// Status maps an error onto the HTTP status code documented in README.md.
func Status(err error) int {
	e, ok := err.(*Error)
	if !ok {
		return 500
	}
	switch e.Code {
	case CodeValidation, CodeFormat:
		return 400
	case CodeNotFound:
		return 404
	case CodeConflict, CodeIdempotency:
		return 409
	default:
		return 500
	}
}

// CodeOf returns the snake_case code of an error, or "internal_error".
func CodeOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return CodeInternal
}
