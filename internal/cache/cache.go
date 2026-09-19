// Package cache wraps Redis for the two things queryforge caches: schema
// introspection, which is stable for minutes and costs three queries against
// the target database, and completed reports keyed by query fingerprint.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

type Cache struct{ rdb *redis.Client }

func New(addr, password string, db int) *Cache {
	return &Cache{rdb: redis.NewClient(&redis.Options{
		Addr: addr, Password: password, DB: db,
		DialTimeout: 3 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
	})}
}

func (c *Cache) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }
func (c *Cache) Close() error                   { return c.rdb.Close() }

var ErrMiss = errors.New("cache miss")

func (c *Cache) Get(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrMiss
	}
	return v, err
}

func (c *Cache) Set(ctx context.Context, key, val string, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, val, ttl).Err()
}

// Key builds a namespaced key from a content hash, keeping SQL text out of
// Redis keys.
func Key(namespace string, parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return namespace + ":" + hex.EncodeToString(h.Sum(nil))[:32]
}
