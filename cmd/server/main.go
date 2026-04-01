package main

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/abhinavtyagiO/ai-gateway-proxy/internal/auth"
	"github.com/abhinavtyagiO/ai-gateway-proxy/internal/cache"
	"github.com/abhinavtyagiO/ai-gateway-proxy/internal/optimizer"
	"github.com/abhinavtyagiO/ai-gateway-proxy/internal/proxy"
	"github.com/abhinavtyagiO/ai-gateway-proxy/internal/security"
	"github.com/redis/go-redis/v9"
)

func main() {
	if err := loadEnvFile(".env"); err != nil {
		log.Fatalf("failed to load environment file: %v", err)
	}

	optimizerClient, err := optimizer.NewClient(optimizerAddr())
	if err != nil {
		log.Fatalf("failed to create optimizer client: %v", err)
	}
	defer optimizerClient.Close()

	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr(),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       0,
	})

	cacheClient := cache.New(rdb)

	proxyHandler, err := proxy.Handler(optimizerClient, cacheClient)
	if err != nil {
		log.Fatalf("failed to build proxy handler: %v", err)
	}

	handler := auth.WithAuth(security.WithScrubber(loggingMiddleware(proxyHandler)), rdb)

	addr := os.Getenv("PORT")
	if addr == "" {
		addr = "8080"
	}

	server := &http.Server{
		Addr:    ":" + addr,
		Handler: handler,
	}

	log.Printf("listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server failed: %v", err)
	}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orgID, _ := auth.OrgIDFromContext(r.Context())
		userID, _ := auth.UserIDFromContext(r.Context())
		log.Printf("incoming method=%s path=%s org_id=%s user_id=%s", r.Method, r.URL.Path, orgID, userID)
		next.ServeHTTP(w, r)
	})
}

func redisAddr() string {
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		return addr
	}

	return "localhost:6379"
}

func optimizerAddr() string {
	if addr := os.Getenv("OPTIMIZER_ADDR"); addr != "" {
		return addr
	}

	return "localhost:50051"
}

func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, found := strings.Cut(line, "=")
		if !found {
			return fmt.Errorf("invalid env line: %q", line)
		}

		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return fmt.Errorf("invalid env line: %q", line)
		}

		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}

	return scanner.Err()
}
