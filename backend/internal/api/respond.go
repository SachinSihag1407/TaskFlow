package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/statemachine"
	"github.com/taskflow/taskflow/internal/store"
)

// envelope is the success response shape: {"data": ..., "meta": ...}.
type envelope struct {
	Data any       `json:"data"`
	Meta *pageMeta `json:"meta,omitempty"`
}

type pageMeta struct {
	Page       int `json:"page"`
	PageSize   int `json:"page_size"`
	Total      int `json:"total"`
	TotalPages int `json:"total_pages"`
}

// apiError is the error response shape.
type apiError struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, data any) { writeJSON(w, http.StatusOK, envelope{Data: data}) }

func created(w http.ResponseWriter, data any) { writeJSON(w, http.StatusCreated, envelope{Data: data}) }

func paged(w http.ResponseWriter, data any, p store.Page, total int) {
	p = p.Normalize()
	pages := (total + p.PageSize - 1) / p.PageSize
	writeJSON(w, http.StatusOK, envelope{Data: data, Meta: &pageMeta{Page: p.Page, PageSize: p.PageSize, Total: total, TotalPages: pages}})
}

func fail(w http.ResponseWriter, r *http.Request, status int, code, msg string, fields map[string]string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg, Fields: fields, RequestID: requestID(r)}})
}

// failErr maps domain errors to HTTP responses. Unexpected errors are logged
// with the request id and reported generically, never leaking internals.
func (s *Server) failErr(w http.ResponseWriter, r *http.Request, err error) {
	var ve *domain.ValidationError
	var ce *domain.ConflictError
	var it *statemachine.ErrInvalidTransition
	switch {
	case errors.As(err, &ve):
		fail(w, r, http.StatusUnprocessableEntity, "validation_failed", "Some fields are invalid.", ve.Fields)
	case errors.As(err, &ce):
		fail(w, r, http.StatusConflict, ce.Code, capitalize(ce.Message)+".", nil)
	case errors.As(err, &it):
		fail(w, r, http.StatusConflict, "invalid_transition", it.Error(), nil)
	case errors.Is(err, domain.ErrNotFound):
		fail(w, r, http.StatusNotFound, "not_found", "Resource not found.", nil)
	case errors.Is(err, domain.ErrForbidden):
		fail(w, r, http.StatusForbidden, "forbidden", "You do not have permission to perform this action.", nil)
	case errors.Is(err, domain.ErrUnauthorized):
		fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.", nil)
	default:
		s.log.Error("request failed", "request_id", requestID(r), "method", r.Method, "path", r.URL.Path, "err", err)
		fail(w, r, http.StatusInternalServerError, "internal_error", "Something went wrong. Try again, and quote the request id if it persists.", nil)
	}
}

// decode reads a JSON body strictly: unknown fields, trailing data and bodies
// over the configured limit are rejected.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		fail(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json.", nil)
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			fail(w, r, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("Request body must be at most %d bytes.", mbe.Limit), nil)
		case errors.Is(err, io.EOF):
			fail(w, r, http.StatusBadRequest, "invalid_json", "Request body is empty.", nil)
		default:
			fail(w, r, http.StatusBadRequest, "invalid_json", "Request body is not valid JSON: "+jsonErr(err), nil)
		}
		return false
	}
	if dec.More() {
		fail(w, r, http.StatusBadRequest, "invalid_json", "Request body has trailing data.", nil)
		return false
	}
	return true
}

func jsonErr(err error) string {
	var se *json.SyntaxError
	var ute *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		return fmt.Sprintf("syntax error at offset %d", se.Offset)
	case errors.As(err, &ute):
		return fmt.Sprintf("field %q must be %s", ute.Field, ute.Type)
	case strings.HasPrefix(err.Error(), "json: unknown field"):
		return strings.TrimPrefix(err.Error(), "json: ")
	}
	return "malformed body"
}

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		fail(w, r, http.StatusNotFound, "not_found", "Resource not found.", nil)
		return uuid.Nil, false
	}
	return id, true
}

// query helpers

func qPage(r *http.Request) store.Page {
	p := store.Page{Page: qInt(r, "page", 1), PageSize: qInt(r, "page_size", 25)}
	return p.Normalize()
}

func qInt(r *http.Request, key string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	return v
}

func qList(r *http.Request, key string) []string {
	var out []string
	for _, v := range r.URL.Query()[key] {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" && len(p) <= 64 {
				out = append(out, p)
			}
		}
	}
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func qString(r *http.Request, key string, max int) string {
	v := strings.TrimSpace(r.URL.Query().Get(key))
	if len(v) > max {
		v = v[:max]
	}
	return v
}

func qUUID(r *http.Request, key string) *uuid.UUID {
	if v := r.URL.Query().Get(key); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			return &id
		}
	}
	return nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
