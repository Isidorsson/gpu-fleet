package remediation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxWebhookBytes bounds a webhook body. Alertmanager truncates a group to
// its own alert limit, so a legitimate payload is far smaller.
const MaxWebhookBytes = 1 << 20

var ErrMalformedWebhook = errors.New("malformed alertmanager webhook")

// ParseWebhook reads one Alertmanager webhook from r. Fields it does not use
// (receiver, groupLabels, externalURL, ...) are ignored, as Alertmanager adds
// them between versions.
func ParseWebhook(r io.Reader) (Webhook, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxWebhookBytes+1))
	if err != nil {
		return Webhook{}, fmt.Errorf("read webhook: %w", err)
	}
	if len(body) > MaxWebhookBytes {
		return Webhook{}, fmt.Errorf("%w: body exceeds %d bytes", ErrMalformedWebhook, MaxWebhookBytes)
	}

	var w Webhook
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&w); err != nil {
		return Webhook{}, fmt.Errorf("%w: %w", ErrMalformedWebhook, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Webhook{}, fmt.Errorf("%w: data after the JSON document", ErrMalformedWebhook)
	}
	if err := checkVersion(w); err != nil {
		return Webhook{}, err
	}
	return w, nil
}

func checkVersion(w Webhook) error {
	if w.Version != WebhookVersion {
		return fmt.Errorf("%w: %q, want %q", ErrUnsupportedVersion, w.Version, WebhookVersion)
	}
	return nil
}
