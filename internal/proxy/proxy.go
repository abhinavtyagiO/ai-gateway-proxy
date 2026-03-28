package proxy

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

const upstreamHost = "api.openai.com"

type jsonError struct {
	Error string `json:"error"`
}

func New() (*httputil.ReverseProxy, error) {
	target, err := url.Parse("https://" + upstreamHost)
	if err != nil {
		return nil, err
	}

	logger := log.New(os.Stderr, "proxy: ", log.LstdFlags|log.Lshortfile)
	rp := httputil.NewSingleHostReverseProxy(target)
	baseDirector := rp.Director

	rp.Director = func(req *http.Request) {
		originalPath := req.URL.Path
		originalRawPath := req.URL.RawPath
		originalQuery := req.URL.RawQuery

		baseDirector(req)

		req.URL.Path = originalPath
		req.URL.RawPath = originalRawPath
		req.URL.RawQuery = originalQuery
		req.Host = upstreamHost
		req.Header.Set("Host", upstreamHost)
	}

	rp.ModifyResponse = func(resp *http.Response) error {
		if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			resp.Header.Set("X-Accel-Buffering", "no")
			resp.Header.Set("Cache-Control", "no-cache")
		}
		return nil
	}

	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Printf("upstream request failed: method=%s path=%s err=%v", r.Method, r.URL.RequestURI(), err)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		if encodeErr := json.NewEncoder(w).Encode(jsonError{
			Error: "upstream OpenAI API is unreachable",
		}); encodeErr != nil {
			logger.Printf("failed to encode error response: %v", encodeErr)
		}
	}

	rp.ErrorLog = logger
	rp.FlushInterval = -1

	return rp, nil
}

func Handler() (http.Handler, error) {
	rp, err := New()
	if err != nil {
		return nil, err
	}

	return rp, nil
}
