package credentials

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

// NormalizeCapturedPayload extracts only credential-shaped scalar values from
// browser storage. Some NewAPI forks keep the token and user id inside a JSON
// object such as {"user":{"id":1,"accessToken":"..."}} instead of using
// top-level access_token/user_id keys.
func NormalizeCapturedPayload(payload domain.CredentialPayload) domain.CredentialPayload {
	payload.LocalStorage = normalizeCapturedStorage(payload.LocalStorage)
	payload.SessionStorage = normalizeCapturedStorage(payload.SessionStorage)

	if strings.TrimSpace(payload.Authorization) == "" {
		if value := firstCapturedValue(payload, "authorization"); value != "" {
			payload.Authorization = value
		} else if value := firstCapturedValue(payload, "access_token", "auth_token", "session_token", "token", "id_token"); value != "" {
			payload.Authorization = "Bearer " + trimBearer(value)
		}
	}
	if payload.UserID == 0 {
		if raw := firstCapturedValue(payload, "user_id", "uid"); raw != "" {
			if parsed, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(raw), `"`), 10, 64); err == nil && parsed > 0 {
				payload.UserID = parsed
			}
		}
	}
	return payload
}

func normalizeCapturedStorage(source map[string]string) map[string]string {
	if len(source) == 0 {
		return source
	}
	result := make(map[string]string, len(source)+6)
	for key, value := range source {
		result[key] = value
	}
	candidates := make(map[string]rankedCapturedValue)
	for key, raw := range source {
		collectCapturedScalar(candidates, []string{key}, key, raw)
		var decoded any
		decoder := json.NewDecoder(bytes.NewBufferString(raw))
		decoder.UseNumber()
		if decoder.Decode(&decoded) == nil {
			walkCapturedValue(candidates, []string{key}, decoded, 0)
		}
	}
	for key, candidate := range candidates {
		if strings.TrimSpace(result[key]) == "" {
			result[key] = candidate.value
		}
	}
	return result
}

type rankedCapturedValue struct {
	value string
	rank  int
}

func walkCapturedValue(output map[string]rankedCapturedValue, path []string, value any, depth int) {
	if depth > 8 {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			walkCapturedValue(output, append(path, key), child, depth+1)
		}
	case []any:
		for _, child := range typed {
			walkCapturedValue(output, path, child, depth+1)
		}
	case string:
		if len(path) > 0 {
			collectCapturedScalar(output, path, path[len(path)-1], typed)
		}
	case json.Number:
		if len(path) > 0 {
			collectCapturedScalar(output, path, path[len(path)-1], typed.String())
		}
	case float64:
		if len(path) > 0 {
			collectCapturedScalar(output, path, path[len(path)-1], strconv.FormatFloat(typed, 'f', -1, 64))
		}
	}
}

func collectCapturedScalar(output map[string]rankedCapturedValue, path []string, key, raw string) {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > 32768 {
		return
	}
	canonical, rank := capturedKey(path, key, value)
	if canonical == "" {
		return
	}
	if current, exists := output[canonical]; !exists || rank > current.rank {
		output[canonical] = rankedCapturedValue{value: value, rank: rank}
	}
}

func capturedKey(path []string, key, value string) (string, int) {
	normalized := normalizeCapturedKey(key)
	switch normalized {
	case "authorization":
		return "authorization", 100
	case "accesstoken":
		return "access_token", 95
	case "authtoken":
		return "auth_token", 90
	case "sessiontoken":
		return "session_token", 85
	case "token":
		if credentialToken(value) {
			return "token", 75
		}
	case "idtoken":
		return "id_token", 65
	case "refreshtoken":
		return "refresh_token", 90
	case "userid":
		return "user_id", 100
	case "uid":
		return "uid", 90
	case "id":
		if capturedPathHasUserContext(path[:len(path)-1]) {
			return "user_id", 70
		}
	case "expiresat", "expiry", "expiration":
		return "expires_at", 80
	case "expiresin":
		return "expires_in", 80
	}
	return "", 0
}

func normalizeCapturedKey(value string) string {
	return strings.Map(func(character rune) rune {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			return unicode.ToLower(character)
		}
		return -1
	}, value)
}

func capturedPathHasUserContext(path []string) bool {
	for _, part := range path {
		switch normalizeCapturedKey(part) {
		case "user", "currentuser", "account", "profile", "member", "self", "userinfo":
			return true
		}
	}
	return false
}

func credentialToken(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 8 || strings.ContainsAny(value, "\r\n\t ") {
		return false
	}
	return !strings.EqualFold(value, "undefined") && !strings.EqualFold(value, "null")
}

func firstCapturedValue(payload domain.CredentialPayload, keys ...string) string {
	for _, values := range []map[string]string{payload.LocalStorage, payload.SessionStorage} {
		for _, key := range keys {
			if value := strings.TrimSpace(values[key]); value != "" {
				return value
			}
		}
	}
	return ""
}
