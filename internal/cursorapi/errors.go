package cursorapi

import (
	"errors"
	"fmt"
	"net/http"
)

// APIError is a non-2xx response from the fleet API.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
	}
	return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
}

// Sentinel errors. Use errors.Is; they wrap the underlying *APIError.
var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrUnauthorized  = errors.New("unauthorized")
	ErrCursorExpired = errors.New("stream cursor expired")
)

// Is lets errors.Is(apiErr, ErrNotFound) etc. work directly on *APIError.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrConflict:
		return e.Status == http.StatusConflict
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
	case ErrCursorExpired:
		return e.Status == http.StatusGone
	}
	return false
}

// StatusOf returns the HTTP status carried by err, or 0.
func StatusOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// IsAuthError reports whether the error is a 401/403 (bad or wrong-kind key).
func IsAuthError(err error) bool { return errors.Is(err, ErrUnauthorized) }
