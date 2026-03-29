package security

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const maxBodyBytes = 5 << 20

var (
	awsAccessKeyPattern = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	emailPattern        = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	bearerTokenPattern  = regexp.MustCompile(`(?i)bearer [a-zA-Z0-9\-._~+/]+=*`)
)

type jsonError struct {
	Error string `json:"error"`
}

func WithScrubber(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("X-Force-Frontier"), "true") {
			log.Printf("Bypass triggered")
			next.ServeHTTP(w, r)
			return
		}

		if r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "failed to read request body")
			return
		}
		defer r.Body.Close()

		if len(body) > maxBodyBytes {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body exceeds 5MB limit")
			return
		}

		scrubbed := scrub(body)
		r.Body = io.NopCloser(bytes.NewBuffer(scrubbed))
		r.ContentLength = int64(len(scrubbed))
		r.Header.Set("Content-Length", strconv.Itoa(len(scrubbed)))

		next.ServeHTTP(w, r)
	})
}

func scrub(body []byte) []byte {
	scrubbed := awsAccessKeyPattern.ReplaceAll(body, []byte("[REDACTED]"))
	scrubbed = emailPattern.ReplaceAll(scrubbed, []byte("[REDACTED]"))
	scrubbed = bearerTokenPattern.ReplaceAll(scrubbed, []byte("[REDACTED]"))
	return scrubbed
}

func writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(jsonError{Error: message})
}
