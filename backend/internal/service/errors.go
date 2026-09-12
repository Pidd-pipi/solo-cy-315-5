package service

import "errors"

var (
	// ErrNotFound indicates a requested record does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalid indicates malformed business input.
	ErrInvalid = errors.New("invalid input")
	// ErrConflict indicates a constraint violation.
	ErrConflict = errors.New("resource conflict")
)

// BusinessError is a sentinel error carrying an explanatory message and
// optional structured data (e.g. the conflicts that rejected a publish).
// Handler code can surface Message/Data while still using errors.Is for
// status mapping.
type BusinessError struct {
	Sentinel error
	Message  string
	Data     any
}

func (e *BusinessError) Error() string { return e.Message }

func (e *BusinessError) Unwrap() error { return e.Sentinel }

// InvalidWithData wraps ErrInvalid with a human-readable message and data.
func InvalidWithData(message string, data any) error {
	return &BusinessError{Sentinel: ErrInvalid, Message: message, Data: data}
}

// ConflictWithData wraps ErrConflict with a human-readable message and data.
func ConflictWithData(message string, data any) error {
	return &BusinessError{Sentinel: ErrConflict, Message: message, Data: data}
}
