package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ProblemDocument is the application/problem+json body every failed route
// returns (schema ProblemDocument: type, title, status, detail, code,
// request_id).
type ProblemDocument struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Code      string `json:"code"`
	RequestID string `json:"request_id"`
}

// HTTPError is a non-2xx response. Its message carries the status, the
// problem code and the request id; the detail is passed through redactErrorBody
// so a server that echoes a credential cannot leak it into a log line.
type HTTPError struct {
	Method     string
	URL        string
	StatusCode int
	Problem    ProblemDocument
	// Body is the redacted up-to-8 KiB body when it was not a problem document.
	Body string
	// RetryAfter is the Retry-After header, unparsed.
	RetryAfter string
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "bridge: %s %s: http %d", e.Method, e.URL, e.StatusCode)
	if e.Problem.Code != "" {
		fmt.Fprintf(&b, " %s", e.Problem.Code)
	}
	detail := redactErrorBody(e.Problem.Detail)
	if detail == "" {
		detail = redactErrorBody(e.Body)
	}
	if detail != "" {
		if len(detail) > 400 {
			detail = detail[:400] + "...(truncated)"
		}
		fmt.Fprintf(&b, ": %s", detail)
	}
	if e.Problem.RequestID != "" {
		fmt.Fprintf(&b, " [request_id=%s]", e.Problem.RequestID)
	}
	return b.String()
}

// Is makes every HTTPError match on status class where useful: a 404 matches
// ErrNotFound, 409 matches ErrConflict, 412 matches ErrPreconditionFailed.
func (e *HTTPError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrConflict:
		return e.StatusCode == http.StatusConflict
	case ErrPreconditionFailed:
		return e.StatusCode == http.StatusPreconditionFailed
	case ErrPreconditionRequired:
		return e.StatusCode == http.StatusPreconditionRequired
	}
	return false
}

// Sentinel status errors, reachable through errors.Is on any *HTTPError.
var (
	ErrNotFound             = errors.New("bridge: not found")
	ErrConflict             = errors.New("bridge: conflict")
	ErrPreconditionFailed   = errors.New("bridge: stale etag")
	ErrPreconditionRequired = errors.New("bridge: If-Match required")
)

// PreflightError is a failure detected before a request left the process:
// insecure origin, origin outside the allowlist, missing bearer.
type PreflightError struct {
	Reason error
	Detail string
	Origin string
}

func (e *PreflightError) Error() string {
	return fmt.Sprintf("%v: %s", e.Reason, e.Detail)
}

func (e *PreflightError) Is(target error) bool { return target == e.Reason }

// DecodeError reports a response body that could not be decoded into the
// expected schema.
type DecodeError struct {
	Method string
	URL    string
	What   string
	Err    error
}

func (e *DecodeError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("bridge: %s %s: decode %s: %v", e.Method, e.URL, e.What, e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// Is treats a truncated body as ErrOutputTruncated; nothing else matches.
func (e *DecodeError) Is(target error) bool {
	return target == ErrOutputTruncated && errors.Is(e.Err, io.ErrUnexpectedEOF)
}

// redactErrorBody bounds and scrubs a server-supplied string before it can
// reach an error message. It removes bearer-shaped material rather than trying
// to know every token format.
func redactErrorBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > 8192 {
		s = s[:8192] + "...(truncated)"
	}
	lower := strings.ToLower(s)
	for _, marker := range []string{"authorization:", "bearer "} {
		idx := 0
		for {
			rel := strings.Index(lower[idx:], marker)
			if rel < 0 {
				break
			}
			start := idx + rel
			end := start + len(marker)
			for end < len(s) && s[end] != ' ' && s[end] != '\n' && s[end] != '\r' &&
				s[end] != '"' && s[end] != ',' && s[end] != '}' {
				end++
			}
			s = s[:start] + "[redacted]" + s[end:]
			lower = strings.ToLower(s)
			idx = start + len("[redacted]")
			if idx >= len(s) {
				break
			}
		}
	}
	return s
}

// parseProblem turns a failed response into an *HTTPError, preferring the
// problem document and falling back to the redacted raw body.
func parseProblem(resp *http.Response, method, url string, body []byte) *HTTPError {
	e := &HTTPError{
		Method:     method,
		URL:        url,
		StatusCode: resp.StatusCode,
		RetryAfter: resp.Header.Get("Retry-After"),
	}
	if len(body) == 0 {
		return e
	}
	// A gateway HTML page can be arbitrary: try for a problem document, then
	// fall back to the redacted raw body so the message still says something.
	var problem ProblemDocument
	if err := json.Unmarshal(body, &problem); err == nil && (problem.Code != "" || problem.Title != "" || problem.Detail != "") {
		e.Problem = problem
		return e
	}
	e.Body = string(body)
	return e
}
