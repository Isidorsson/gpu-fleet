package deployagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const (
	maxBodyBytes = 1 << 20
	// maxEchoLen bounds how much of a remote-supplied value goes into an error.
	maxEchoLen = 32
)

var errBodyTooLarge = errors.New("response body too large")

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	req, err := c.newRequest(ctx, method, path, in)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return transportError(ctx, method, path, err)
	}
	defer closeBody(resp.Body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return statusError(method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return decodeBody(ctx, resp.Body, method, path, out)
}

func (c *Client) newRequest(ctx context.Context, method, path string, in any) (*http.Request, error) {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("encode %s %s body: %w", method, path, err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build %s %s request: %w", method, path, err)
	}
	req.SetBasicAuth(c.user, c.password)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func decodeBody(ctx context.Context, r io.Reader, method, path string, out any) error {
	data, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return transportError(ctx, method, path, err)
	}
	if len(data) > maxBodyBytes {
		return fmt.Errorf("%s %s: %w", method, path, errBodyTooLarge)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}

func statusError(method, path string, code int) error {
	switch code {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %s %s: status %d", ErrUnauthorized, method, path, code)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s %s: status %d", ErrNotFound, method, path, code)
	case http.StatusServiceUnavailable:
		return fmt.Errorf("%w: %s %s: status %d", ErrTransient, method, path, code)
	}
	return fmt.Errorf("%s %s: status %d", method, path, code)
}

// transportError reports the caller's own cancellation as such. The cause of a
// transient failure is deliberately not wrapped: an http.Client timeout
// matches context.DeadlineExceeded, which would be mistaken for the caller's.
func transportError(ctx context.Context, method, path string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s %s: %w", method, path, ctxErr)
	}
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		err = urlErr.Err
	}
	return fmt.Errorf("%w: %s %s: %v", ErrTransient, method, path, err)
}

// closeBody drains what is left so the connection can be reused.
func closeBody(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxBodyBytes))
	_ = body.Close()
}
