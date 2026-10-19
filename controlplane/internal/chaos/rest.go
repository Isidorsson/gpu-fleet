package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	requestTimeout   = 10 * time.Second
	maxResponseBytes = 1 << 20
	maxErrorText     = 200
)

// errTransport marks failures to get any HTTP answer, which are worth retrying.
var errTransport = errors.New("transport error")

// StatusError is a non-2xx answer. Body is kept whole so callers can read
// structured error payloads such as the 409 capacity answer.
type StatusError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	text := strings.TrimSpace(e.Body)
	if len(text) > maxErrorText {
		text = text[:maxErrorText] + "..."
	}
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.Path, e.Status, text)
}

// hasStatus reports whether err is a StatusError with the given status.
func hasStatus(err error, status int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == status
}

// transient reports whether retrying the same request may succeed.
func transient(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status >= http.StatusInternalServerError ||
			se.Status == http.StatusTooManyRequests || se.Status == http.StatusRequestTimeout
	}
	return errors.Is(err, errTransport)
}

type rest struct {
	base string
	user string
	pass string
	hc   *http.Client
}

func newREST(base, user, pass string) *rest {
	return &rest{base: base, user: user, pass: pass, hc: &http.Client{Timeout: requestTimeout}}
}

// do sends in as JSON (when non-nil) and decodes a 2xx answer into out (when
// non-nil).
func (r *rest) do(ctx context.Context, method, path string, in, out any) error {
	req, err := r.newRequest(ctx, method, path, in)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %w", errTransport, method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%w: %s %s: read body: %w", errTransport, method, path, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return &StatusError{Method: method, Path: path, Status: resp.StatusCode, Body: string(data)}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

func (r *rest) newRequest(ctx context.Context, method, path string, in any) (*http.Request, error) {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.user != "" {
		req.SetBasicAuth(r.user, r.pass)
	}
	return req, nil
}
