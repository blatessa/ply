// Package plyerr is the error type shared by the server, client and CLI. An
// error says what went wrong and, when there is one, the command that fixes
// it, so people and agents can act on it directly.
package plyerr

import (
	"errors"
	"fmt"
)

// Codes classify errors for programs (and HTTP status mapping).
const (
	NotFound  = "not_found"
	Invalid   = "invalid"
	Stale     = "stale"    // someone else shared newer changes
	Conflict  = "conflict" // a merge has conflicts, or unresolved conflicts remain
	State     = "state"    // the task's status doesn't allow it
	Forbidden = "forbidden"
	Internal  = "internal"
)

// Error is a Ply error.
type Error struct {
	Code string `json:"code"`
	Msg  string `json:"error"`
	// Fix is the command that fixes it, if any, e.g. "ply refresh".
	Fix string `json:"fix,omitempty"`
	// Paths lists the files involved, for conflicts.
	Paths []string `json:"paths,omitempty"`
}

func (e *Error) Error() string {
	if e.Fix != "" {
		return fmt.Sprintf("%s: run `%s`", e.Msg, e.Fix)
	}
	return e.Msg
}

// New makes an error.
func New(code, fix, format string, args ...any) *Error {
	return &Error{Code: code, Fix: fix, Msg: fmt.Sprintf(format, args...)}
}

// As returns err as an *Error, if it is one.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// Is reports whether err is a Ply error with the given code.
func Is(err error, code string) bool {
	e, ok := As(err)
	return ok && e.Code == code
}
