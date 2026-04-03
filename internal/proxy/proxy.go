package proxy

import (
	"bytes"
	"context"
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
	"sync"
	"time"

	"github.com/abhinavtyagiO/ai-gateway-proxy/internal/auth"
	"github.com/abhinavtyagiO/ai-gateway-proxy/internal/cache"
	"github.com/abhinavtyagiO/ai-gateway-proxy/internal/optimizer"
	"github.com/redis/go-redis/v9"
)

const (
	upstreamHost  = "api.openai.com"
	cacheTTL      = 5 * time.Minute
	cacheChunkSz  = 256
	updateQueueSz = 1000
	updateTimeout = 30 * time.Second
)

type jsonError struct {
	Error string `json:"error"`
}

type chatCompletionRequest struct {
	Model    string          `json:"model"`
	Prompt   string          `json:"prompt,omitempty"`
	Messages []chatMessage   `json:"messages,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Stream   bool            `json:"stream,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type preparedRequest struct {
	cacheKey       string
	prompt         string
	model          string
	stream         bool
	cachedResponse string
}

type responseRecorder struct {
	http.ResponseWriter
	body       bytes.Buffer
	statusCode int
}

type cacheUpdateTask struct {
	cacheKey     string
	prompt       string
	responseJSON string
}

type proxyContextKey string

const (
	cacheKeyContextKey proxyContextKey = "cache_key"
	promptContextKey   proxyContextKey = "prompt"
	modelContextKey    proxyContextKey = "model"
)

type streamingAggregator struct {
	source        io.ReadCloser
	logger        *log.Logger
	cacheClient   *cache.Cache
	cacheKey      string
	prompt        string
	model         string
	eventBuffer   bytes.Buffer
	contentBuffer strings.Builder
	doneOnce      sync.Once
}

var (
	updateQueue      = make(chan *cacheUpdateTask, updateQueueSz)
	cacheWorkersOnce sync.Once
)

func New(optimizerClient *optimizer.OptimizerClient, cacheClient *cache.Cache) (*httputil.ReverseProxy, error) {
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
			cacheKey, _ := resp.Request.Context().Value(cacheKeyContextKey).(string)
			prompt, _ := resp.Request.Context().Value(promptContextKey).(string)
			model, _ := resp.Request.Context().Value(modelContextKey).(string)
			if cacheKey != "" && prompt != "" {
				resp.Body = newStreamingAggregator(resp.Body, logger, cacheClient, optimizerClient, cacheKey, prompt, model)
			}
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

func Handler(optimizerClient *optimizer.OptimizerClient, cacheClient *cache.Cache) (http.Handler, error) {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		return nil, errors.New("OPENAI_API_KEY is not set")
	}

	startCacheWorkers(4, optimizerClient, cacheClient)

	rp, err := New(optimizerClient, cacheClient)
	if err != nil {
		return nil, err
	}

	logger := log.New(os.Stderr, "proxy: ", log.LstdFlags|log.Lshortfile)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prepared, err := prepareRequest(r, optimizerClient, cacheClient, logger)
		if err != nil {
			logger.Printf("request preparation failed open: method=%s path=%s err=%v", r.Method, r.URL.Path, err)
		}

		if prepared.prompt != "" {
			ctx := context.WithValue(r.Context(), cacheKeyContextKey, prepared.cacheKey)
			ctx = context.WithValue(ctx, promptContextKey, prepared.prompt)
			ctx = context.WithValue(ctx, modelContextKey, prepared.model)
			*r = *r.WithContext(ctx)
		}

		if prepared.cachedResponse != "" {
			streamCacheHit(w, prepared.cachedResponse)
			return
		}

		if prepared.cacheKey == "" || prepared.stream {
			rp.ServeHTTP(w, r)
			return
		}

		recorder := &responseRecorder{ResponseWriter: w}
		rp.ServeHTTP(recorder, r)

		if cacheClient != nil && recorder.statusCode >= http.StatusOK && recorder.statusCode < http.StatusMultipleChoices && recorder.body.Len() > 0 {
			if err := cacheClient.Set(r.Context(), prepared.cacheKey, recorder.body.String(), cacheTTL); err != nil {
				logger.Printf("failed to store L1 cache: method=%s path=%s key=%s err=%v", r.Method, r.URL.Path, prepared.cacheKey, err)
			}
		}
	}), nil
}

func prepareRequest(req *http.Request, optimizerClient *optimizer.OptimizerClient, cacheClient *cache.Cache, logger *log.Logger) (preparedRequest, error) {
	if req.Body == nil {
		return preparedRequest{}, nil
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return preparedRequest{}, err
	}
	defer req.Body.Close()

	restoreRequestBody(req, body)
	if len(body) == 0 {
		return preparedRequest{}, nil
	}

	var payload chatCompletionRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		return preparedRequest{}, err
	}

	orgID, _ := auth.OrgIDFromContext(req.Context())
	userID, _ := auth.UserIDFromContext(req.Context())
	prompt := extractPrompt(payload)
	if orgID == "" || payload.Model == "" || prompt == "" {
		return preparedRequest{stream: payload.Stream}, nil
	}

	cacheKey := cache.GenerateKey(prompt, payload.Model, orgID)
	if cacheClient != nil {
		cachedValue, err := cacheClient.Get(req.Context(), cacheKey)
		if err == nil && cachedValue != "" {
			logger.Printf("L1 cache hit: method=%s path=%s org_id=%s key=%s", req.Method, req.URL.Path, orgID, cacheKey)
			return preparedRequest{cacheKey: cacheKey, prompt: prompt, model: payload.Model, stream: payload.Stream, cachedResponse: cachedValue}, nil
		}
		if errors.Is(err, redis.Nil) {
			logger.Printf("L1 cache miss: method=%s path=%s org_id=%s key=%s", req.Method, req.URL.Path, orgID, cacheKey)
		}
		if err != nil && !errors.Is(err, redis.Nil) {
			logger.Printf("L1 cache lookup failed: method=%s path=%s key=%s err=%v", req.Method, req.URL.Path, cacheKey, err)
		}
	}

	if optimizerClient == nil || userID == "" {
		return preparedRequest{cacheKey: cacheKey, prompt: prompt, model: payload.Model, stream: payload.Stream}, nil
	}

	response, err := optimizerClient.Optimize(req.Context(), prompt, payload.Model, userID, orgID)
	if err != nil {
		return preparedRequest{cacheKey: cacheKey, prompt: prompt, model: payload.Model, stream: payload.Stream}, err
	}

	if response.GetCachedResponse() != "" {
		if cacheClient != nil {
			if err := cacheClient.Set(req.Context(), cacheKey, response.GetCachedResponse(), cacheTTL); err != nil {
				logger.Printf("failed to backfill L1 cache from optimizer: method=%s path=%s key=%s err=%v", req.Method, req.URL.Path, cacheKey, err)
			}
		}
		logger.Printf("L2 cache hit: method=%s path=%s org_id=%s key=%s", req.Method, req.URL.Path, orgID, cacheKey)
		return preparedRequest{cacheKey: cacheKey, prompt: prompt, model: payload.Model, stream: payload.Stream, cachedResponse: response.GetCachedResponse()}, nil
	}

	updated, changed := applyOptimization(payload, response.GetOptimizedPrompt(), response.GetTargetModel())
	if !changed {
		return preparedRequest{cacheKey: cacheKey, prompt: prompt, model: payload.Model, stream: payload.Stream}, nil
	}

	updatedBody, err := json.Marshal(updated)
	if err != nil {
		return preparedRequest{cacheKey: cacheKey, stream: payload.Stream}, err
	}

	restoreRequestBody(req, updatedBody)
	logger.Printf("optimizer applied: method=%s path=%s org_id=%s user_id=%s target_model=%s", req.Method, req.URL.Path, orgID, userID, updated.Model)
	return preparedRequest{cacheKey: cacheKey, prompt: prompt, model: updated.Model, stream: updated.Stream}, nil
}

func restoreRequestBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

func streamCacheHit(w http.ResponseWriter, cachedJSON string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		_, _ = io.WriteString(w, cachedJSON)
		return
	}

	for start := 0; start < len(cachedJSON); start += cacheChunkSz {
		end := start + cacheChunkSz
		if end > len(cachedJSON) {
			end = len(cachedJSON)
		}

		chunk := strings.ReplaceAll(cachedJSON[start:end], "\n", "\ndata: ")
		_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
		flusher.Flush()
	}

	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (rr *responseRecorder) Header() http.Header {
	return rr.ResponseWriter.Header()
}

func (rr *responseRecorder) WriteHeader(statusCode int) {
	rr.statusCode = statusCode
	rr.ResponseWriter.WriteHeader(statusCode)
}

func (rr *responseRecorder) Write(data []byte) (int, error) {
	if rr.statusCode == 0 {
		rr.statusCode = http.StatusOK
	}
	rr.body.Write(data)
	return rr.ResponseWriter.Write(data)
}

func (rr *responseRecorder) Flush() {
	if flusher, ok := rr.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func newStreamingAggregator(source io.ReadCloser, logger *log.Logger, cacheClient *cache.Cache, optimizerClient *optimizer.OptimizerClient, cacheKey, prompt, model string) io.ReadCloser {
	return &streamingAggregator{
		source:      source,
		logger:      logger,
		cacheClient: cacheClient,
		cacheKey:    cacheKey,
		prompt:      prompt,
		model:       model,
	}
}

func (sr *streamingAggregator) Read(p []byte) (int, error) {
	n, err := sr.source.Read(p)
	if n > 0 {
		chunk := p[:n]
		sr.consume(chunk)
	}
	if errors.Is(err, io.EOF) {
		sr.triggerDone()
	}
	return n, err
}

func (sr *streamingAggregator) Close() error {
	sr.triggerDone()
	return sr.source.Close()
}

func (sr *streamingAggregator) consume(chunk []byte) {
	_, _ = sr.eventBuffer.Write(chunk)
	for {
		data := sr.eventBuffer.Bytes()
		idx := bytes.Index(data, []byte("\n\n"))
		if idx < 0 {
			return
		}

		event := string(data[:idx])
		remaining := append([]byte(nil), data[idx+2:]...)
		sr.eventBuffer.Reset()
		_, _ = sr.eventBuffer.Write(remaining)
		sr.processEvent(event)
	}
}

func (sr *streamingAggregator) processEvent(event string) {
	var dataLines []string
	for _, line := range strings.Split(event, "\n") {
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}
	if len(dataLines) == 0 {
		return
	}

	data := strings.Join(dataLines, "\n")
	if data == "[DONE]" {
		sr.triggerDone()
		return
	}

	var payload struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return
	}

	for _, choice := range payload.Choices {
		if choice.Delta.Content != "" {
			sr.contentBuffer.WriteString(choice.Delta.Content)
		}
	}
}

func (sr *streamingAggregator) triggerDone() {
	sr.doneOnce.Do(func() {
		if sr.cacheKey == "" || sr.prompt == "" {
			return
		}

		responseJSON, err := json.Marshal(map[string]any{
			"model": sr.model,
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": sr.contentBuffer.String(),
					},
				},
			},
		})
		if err != nil {
			sr.logger.Printf("failed to marshal streaming cache payload: key=%s err=%v", sr.cacheKey, err)
			return
		}

		cacheKey := sr.cacheKey
		prompt := sr.prompt
		responseText := string(responseJSON)
		if sr.cacheClient != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if err := sr.cacheClient.Set(ctx, cacheKey, responseText, cacheTTL); err != nil {
				sr.logger.Printf("failed to store streamed L1 cache: key=%s err=%v", cacheKey, err)
			}
			cancel()
		}

		updateQueue <- &cacheUpdateTask{
			cacheKey:     cacheKey,
			prompt:       prompt,
			responseJSON: responseText,
		}
	})
}

func startCacheWorkers(count int, optimizerClient *optimizer.OptimizerClient, cacheClient *cache.Cache) {
	cacheWorkersOnce.Do(func() {
		logger := log.New(os.Stderr, "proxy: ", log.LstdFlags|log.Lshortfile)
		for i := 0; i < count; i++ {
			go func(workerID int) {
				for task := range updateQueue {
					if task == nil || optimizerClient == nil {
						continue
					}

					ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
					_, err := optimizerClient.UpdateCache(ctx, task.prompt, task.responseJSON)
					cancel()
					if err != nil {
						logger.Printf("cache worker failed: worker=%d key=%s err=%v", workerID, task.cacheKey, err)
						continue
					}

					logger.Printf("cache worker updated L2 cache: worker=%d key=%s", workerID, task.cacheKey)
				}
			}(i + 1)
		}
	})
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
