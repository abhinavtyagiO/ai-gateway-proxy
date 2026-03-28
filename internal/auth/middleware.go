package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/redis/go-redis/v9"
)

type contextKey string

const (
	orgIDContextKey  contextKey = "org_id"
	userIDContextKey contextKey = "user_id"
)

type tokenRecord struct {
	OrgID  string `json:"org_id"`
	UserID string `json:"user_id"`
	Tier   string `json:"tier"`
}

type jsonError struct {
	Error string `json:"error"`
}

func WithAuth(next http.Handler, rdb *redis.Client) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rdb == nil {
			writeUnauthorized(w, "authorization backend unavailable")
			return
		}

		token, err := bearerToken(r.Header.Get("Authorization"))
		if err != nil {
			writeUnauthorized(w, "missing or malformed bearer token")
			return
		}

		record, err := lookupToken(r.Context(), rdb, token)
		if err != nil {
			writeUnauthorized(w, "invalid API token")
			return
		}

		ctx := context.WithValue(r.Context(), orgIDContextKey, record.OrgID)
		ctx = context.WithValue(ctx, userIDContextKey, record.UserID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func OrgIDFromContext(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(orgIDContextKey).(string)
	return value, ok
}

func UserIDFromContext(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(userIDContextKey).(string)
	return value, ok
}

func bearerToken(header string) (string, error) {
	if header == "" {
		return "", errors.New("authorization header is required")
	}

	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errors.New("authorization header must use bearer scheme")
	}

	token := strings.TrimSpace(parts[1])
	if !isValidTokenFormat(token) {
		return "", errors.New("token format is invalid")
	}

	return token, nil
}

func isValidTokenFormat(token string) bool {
	parts := strings.Split(token, "-")
	if len(parts) != 4 || parts[0] != "sk" {
		return false
	}

	for _, part := range parts[1:] {
		if strings.TrimSpace(part) == "" {
			return false
		}
	}

	return true
}

func lookupToken(ctx context.Context, rdb *redis.Client, token string) (tokenRecord, error) {
	payload, err := rdb.Get(ctx, token).Result()
	if err != nil {
		return tokenRecord{}, err
	}

	var record tokenRecord
	if err := json.Unmarshal([]byte(payload), &record); err != nil {
		return tokenRecord{}, err
	}

	if record.OrgID == "" || record.UserID == "" || record.Tier == "" {
		return tokenRecord{}, errors.New("token record is incomplete")
	}

	return record, nil
}

func writeUnauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(jsonError{Error: message})
}
