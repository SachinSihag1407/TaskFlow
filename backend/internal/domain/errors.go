package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrForbidden    = errors.New("forbidden")
	ErrUnauthorized = errors.New("unauthorized")
)

// ConflictError signals that the request is valid but cannot be applied to
// the resource's current state (stale version, invalid transition, etc.).
type ConflictError struct {
	Code    string
	Message string
}

func (e *ConflictError) Error() string { return e.Message }

func Conflict(code, format string, args ...any) error {
	return &ConflictError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ValidationError carries per-field messages for 422 responses.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+e.Fields[k])
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// Validator accumulates field errors.
type Validator struct{ fields map[string]string }

func (v *Validator) Add(field, msg string) {
	if v.fields == nil {
		v.fields = map[string]string{}
	}
	if _, exists := v.fields[field]; !exists {
		v.fields[field] = msg
	}
}

func (v *Validator) Check(ok bool, field, msg string) {
	if !ok {
		v.Add(field, msg)
	}
}

func (v *Validator) Err() error {
	if len(v.fields) == 0 {
		return nil
	}
	return &ValidationError{Fields: v.fields}
}
