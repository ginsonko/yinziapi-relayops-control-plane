package targets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
)

const newAPIRootRole = 100

type NewAPILoginResult struct {
	Username string                   `json:"username"`
	UserID   int64                    `json:"user_id"`
	Role     int                      `json:"role"`
	Payload  domain.CredentialPayload `json:"-"`
}

// AuthenticateNewAPI exchanges a root username/password for a same-origin
// session. The password is never returned and callers only persist Payload.
func (f *Factory) AuthenticateNewAPI(ctx context.Context, baseURL, username, password string) (NewAPILoginResult, error) {
	if err := f.RemoteWriteGate().Check("target administrator login"); err != nil {
		return NewAPILoginResult{}, err
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if _, err := netguard.ParseOrigin(baseURL); err != nil {
		return NewAPILoginResult{}, err
	}
	if strings.TrimSpace(username) == "" || password == "" {
		return NewAPILoginResult{}, errors.New("NewAPI username and password are required")
	}
	client, err := netguard.SameOriginClient(f.client, baseURL)
	if err != nil {
		return NewAPILoginResult{}, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return NewAPILoginResult{}, err
	}
	client.Jar = jar
	body, err := json.Marshal(map[string]string{"username": strings.TrimSpace(username), "password": password})
	if err != nil {
		return NewAPILoginResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/user/login", bytes.NewReader(body))
	if err != nil {
		return NewAPILoginResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return NewAPILoginResult{}, fmt.Errorf("log in to NewAPI: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return NewAPILoginResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return NewAPILoginResult{}, fmt.Errorf("NewAPI login returned %s", resp.Status)
	}
	var envelope struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			ID         int64  `json:"id"`
			Username   string `json:"username"`
			Role       int    `json:"role"`
			Require2FA bool   `json:"require_2fa"`
		} `json:"data"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return NewAPILoginResult{}, fmt.Errorf("decode NewAPI login response: %w", err)
	}
	if !envelope.Success {
		if strings.TrimSpace(envelope.Message) == "" {
			envelope.Message = "NewAPI rejected the login"
		}
		return NewAPILoginResult{}, errors.New(envelope.Message)
	}
	if envelope.Data.Require2FA {
		return NewAPILoginResult{}, errors.New("this NewAPI account requires 2FA; use browser pairing after completing 2FA")
	}
	if envelope.Data.ID <= 0 || envelope.Data.Role < newAPIRootRole {
		return NewAPILoginResult{}, errors.New("the NewAPI account is not a root administrator")
	}
	parsed, _ := url.Parse(baseURL)
	cookies := jar.Cookies(parsed)
	if len(cookies) == 0 {
		return NewAPILoginResult{}, errors.New("NewAPI login succeeded but returned no reusable session cookie")
	}
	sort.Slice(cookies, func(i, j int) bool { return cookies[i].Name < cookies[j].Name })
	parts := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if strings.ContainsAny(cookie.Name+cookie.Value, "\r\n") {
			continue
		}
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	if len(parts) == 0 {
		return NewAPILoginResult{}, errors.New("NewAPI session cookie was empty")
	}
	return NewAPILoginResult{Username: envelope.Data.Username, UserID: envelope.Data.ID, Role: envelope.Data.Role,
		Payload: domain.CredentialPayload{UserID: envelope.Data.ID, Cookie: strings.Join(parts, "; "), CapturedOrigin: baseURL}}, nil
}
