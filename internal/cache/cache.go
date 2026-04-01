package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/redis/go-redis/v9"
)

type Cache struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Cache {
	return &Cache{rdb: rdb}
}

func (c *Cache) Get(ctx context.Context, key string) (string, error) {
	if c == nil || c.rdb == nil {
		return "", redis.Nil
	}

	return c.rdb.Get(ctx, key).Result()
}

func (c *Cache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if c == nil || c.rdb == nil {
		return nil
	}

	return c.rdb.Set(ctx, key, value, ttl).Err()
}

func GenerateKey(prompt, model, orgID string) string {
	sum := sha256.Sum256([]byte(orgID + ":" + model + ":" + prompt))
	return hex.EncodeToString(sum[:])
}
