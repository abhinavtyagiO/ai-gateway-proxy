package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/abhinavtyagiO/ai-gateway-proxy/internal/auth"
	"github.com/abhinavtyagiO/ai-gateway-proxy/internal/optimizer"
)

const upstreamHost = "api.openai.com"

type jsonError struct {
	Error string `json:"error"`
}

type chatCompletionRequest struct {
	Model    string          `json:"model"`
	Prompt   string          `json:"prompt,omitempty"`
	Messages []chatMessage   `json:"messages,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func New(optimizerClient *optimizer.OptimizerClient) (*httputil.ReverseProxy, error) {
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
		apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		if err := optimizeRequest(req, optimizerClient, logger); err != nil {
			logger.Printf("optimizer failed open: method=%s path=%s err=%v", req.Method, req.URL.Path, err)
		}

		baseDirector(req)

		req.URL.Path = originalPath
		req.URL.RawPath = originalRawPath
		req.URL.RawQuery = originalQuery
		req.Host = upstreamHost
		req.Header.Set("Host", upstreamHost)
		req.Header.Del("Authorization")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
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

func Handler(optimizerClient *optimizer.OptimizerClient) (http.Handler, error) {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		return nil, errors.New("OPENAI_API_KEY is not set")
	}

	rp, err := New(optimizerClient)
	if err != nil {
		return nil, err
	}

	return rp, nil
}

func optimizeRequest(req *http.Request, optimizerClient *optimizer.OptimizerClient, logger *log.Logger) error {
	if optimizerClient == nil || req.Body == nil {
		return nil
	}

	orgID, _ := auth.OrgIDFromContext(req.Context())
	userID, _ := auth.UserIDFromContext(req.Context())
	if orgID == "" || userID == "" {
		return errors.New("missing org_id or user_id in request context")
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return err
	}
	defer req.Body.Close()

	req.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) == 0 {
		return nil
	}

	var payload chatCompletionRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		return err
	}

	prompt := extractPrompt(payload)
	if prompt == "" || payload.Model == "" {
		return nil
	}

	response, err := optimizerClient.Optimize(req.Context(), prompt, payload.Model, userID, orgID)
	if err != nil {
		return err
	}

	updated, changed := applyOptimization(payload, response.GetOptimizedPrompt(), response.GetTargetModel())
	if !changed {
		return nil
	}

	scrubbedBytes, err := json.Marshal(updated)
	if err != nil {
		return err
	}

	req.Body = io.NopCloser(bytes.NewReader(scrubbedBytes))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(scrubbedBytes)), nil
	}
	req.ContentLength = int64(len(scrubbedBytes))
	req.Header.Set("Content-Length", strconv.Itoa(len(scrubbedBytes)))
	logger.Printf("optimizer applied: method=%s path=%s org_id=%s user_id=%s target_model=%s", req.Method, req.URL.Path, orgID, userID, updated.Model)
	return nil
}

func extractPrompt(payload chatCompletionRequest) string {
	if payload.Prompt != "" {
		return payload.Prompt
	}

	for i := len(payload.Messages) - 1; i >= 0; i-- {
		if payload.Messages[i].Role == "user" && payload.Messages[i].Content != "" {
			return payload.Messages[i].Content
		}
	}

	return ""
}

func applyOptimization(payload chatCompletionRequest, optimizedPrompt, targetModel string) (chatCompletionRequest, bool) {
	changed := false

	if targetModel != "" && targetModel != payload.Model {
		payload.Model = targetModel
		changed = true
	}

	if optimizedPrompt == "" {
		return payload, changed
	}

	if payload.Prompt != "" {
		payload.Prompt = optimizedPrompt
		return payload, true
	}

	for i := len(payload.Messages) - 1; i >= 0; i-- {
		if payload.Messages[i].Role == "user" {
			payload.Messages[i].Content = optimizedPrompt
			return payload, true
		}
	}

	return payload, changed
}
