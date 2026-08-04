package targets

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// RemoteError preserves the authorization and transport boundary without
// exposing response headers or credential material.
type RemoteError struct {
	Operation  string
	StatusCode int
	Message    string
	Definitive bool
	Err        error
}

func (e *RemoteError) Error() string {
	message := strings.TrimSpace(e.Message)
	if message == "" && e.Err != nil {
		message = e.Err.Error()
	}
	if message == "" {
		message = "remote target request failed"
	}
	if e.Operation == "" {
		return message
	}
	return fmt.Sprintf("%s: %s", e.Operation, message)
}

func (e *RemoteError) Unwrap() error { return e.Err }

func RemoteHTTPStatus(err error) int {
	var remote *RemoteError
	if errors.As(err, &remote) {
		return remote.StatusCode
	}
	return 0
}

func IsDefinitiveRemoteRejection(err error) bool {
	var remote *RemoteError
	return errors.As(err, &remote) && remote.Definitive
}

func remoteHTTPError(operation string, status int, _ []byte) error {
	message := fmt.Sprintf("remote target returned %s (%d)", http.StatusText(status), status)
	if status == http.StatusUnauthorized {
		message = "management credential was not accepted (401)"
	} else if status == http.StatusForbidden {
		message = "management credential lacks permission (403)"
	}
	definitive := status >= http.StatusBadRequest && status < http.StatusInternalServerError &&
		status != http.StatusRequestTimeout && status != http.StatusTooEarly && status != http.StatusTooManyRequests
	return &RemoteError{Operation: operation, StatusCode: status, Message: message, Definitive: definitive}
}

func safeRemoteMessage(message, fallback string) string {
	message = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(message, "\r", " "), "\n", " "))
	lower := strings.ToLower(message)
	for _, marker := range []string{"authorization", "bearer ", "api_key", "api-key", "apikey", "cookie", "secret", "access_token", "refresh_token", "sk-"} {
		if strings.Contains(lower, marker) {
			return fallback
		}
	}
	if message == "" {
		return fallback
	}
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}
