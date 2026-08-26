package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCache is a Cache backed by Redis, for multi-instance deployments.
type RedisCache struct {
	client *redis.Client
	prefix string
}

// NewRedisCache connects to a Redis instance at the given URL
// (e.g. "redis://localhost:6379/0").
func NewRedisCache(url string) (*RedisCache, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opts)
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, err
	}
	return &RedisCache{client: client, prefix: "fitguard:cache:"}, nil
}

func (c *RedisCache) Get(ctx context.Context, key string) ([]byte, bool) {
	val, err := c.client.Get(ctx, c.prefix+key).Bytes()
	if err != nil {
		return nil, false
	}
	return val, true
}

func (c *RedisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return c.client.Set(ctx, c.prefix+key, value, ttl).Err()
}
