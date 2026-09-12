package repository

import "errors"

var (
	// ErrNotFound is returned when a requested record does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConstraint is returned when a database constraint rejects a write.
	ErrConstraint = errors.New("constraint violation")
	// ErrConcurrentUpdate is returned when a conditional update affects no row
	// because another transaction changed the record in the meantime.
	ErrConcurrentUpdate = errors.New("concurrent update conflict")
	// ErrLockBusy is returned when the database lock cannot be acquired.
	ErrLockBusy = errors.New("database lock busy")
)
