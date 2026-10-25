// Package push sends messages to central. There is no buffering: a message
// that cannot be delivered is dropped by the caller.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gpumon/agent/internal/logx"
)

// Pusher delivers one message.
type Pusher interface {
	Push(ctx context.Context, msg any) error
}

// HTTP posts JSON with a bearer token.
type HTTP struct {
	URL    string
	token  string
	Client *http.Client
}

// NewHTTP returns a pusher with the given request timeout.
func NewHTTP(url, token string, timeout time.Duration) *HTTP {
	return &HTTP{URL: url, token: token, Client: &http.Client{Timeout: timeout}}
}

// Push sends msg as an HTTP POST. Errors never contain the token.
func (h *HTTP) Push(ctx context.Context, msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("cannot encode the payload: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("invalid central URL: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.Client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach central: %v", err)
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("central returned HTTP %d: %s", resp.StatusCode, logx.ASCII(strings.TrimSpace(string(text))))
	}
	return nil
}
