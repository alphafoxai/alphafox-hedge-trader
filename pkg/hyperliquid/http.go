package hyperliquid

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type transport struct {
	baseURL string
	http    *http.Client
}

func newTransport(baseURL string, client *http.Client) transport {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return transport{baseURL: baseURL, http: client}
}

func (t transport) post(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("hyperliquid %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hyperliquid %s: HTTP %d: %s", path, resp.StatusCode, truncate(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("hyperliquid %s: decode: %w: %s", path, err, truncate(raw))
	}
	return nil
}

func truncate(b []byte) string {
	if len(b) > 512 {
		return string(b[:512]) + "…"
	}
	return string(b)
}
