package sessionauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

func NewClient(endpoint, managementToken string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(managementToken) == "" {
		return nil, errors.New("valid management URL and token required")
	}
	return &Client{endpoint: strings.TrimRight(endpoint, "/") + "/v1/sessions/validate", token: managementToken,
		http: &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (c *Client) Validate(ctx context.Context, id, token, service, instance string) (Grant, error) {
	body, err := json.Marshal(map[string]string{"session_id": id, "token": token, "service_id": service, "instance_id": instance})
	if err != nil {
		return Grant{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Grant{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Grant{}, ErrUnauthorized
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Grant{}, ErrUnauthorized
	}
	var grant Grant
	if json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&grant) != nil || grant.ID != id || grant.ServiceID != service || grant.InstanceID != instance || grant.DeviceSerial == "" || !time.Now().Before(grant.ExpiresAt) {
		return Grant{}, ErrUnauthorized
	}
	return grant, nil
}

// Watch closes revoked on failed revalidation, expiration or caller cancellation.
// Active remote connections fail closed within the poll interval plus request timeout.
func (c *Client) Watch(ctx context.Context, grant Grant, token string) <-chan struct{} {
	revoked := make(chan struct{})
	go func() {
		defer close(revoked)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		deadline := time.NewTimer(time.Until(grant.ExpiresAt))
		defer deadline.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline.C:
				return
			case <-ticker.C:
				checkCtx, cancel := context.WithDeadline(ctx, grant.ExpiresAt)
				_, err := c.Validate(checkCtx, grant.ID, token, grant.ServiceID, grant.InstanceID)
				cancel()
				if err != nil {
					return
				}
			}
		}
	}()
	return revoked
}
