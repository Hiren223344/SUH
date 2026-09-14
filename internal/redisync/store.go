package redisync

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// RedisStore adapts a go-redis client to the counterStore interface.
type RedisStore struct {
	rdb *redis.Client
}

// NewRedisStore parses url (a redis:// or rediss:// connection string) and
// returns a ready store. It does not block on connecting; a bad address
// surfaces on the first IncrByFloat/Get call, which the Syncer already
// treats as a non-fatal, logged failure.
func NewRedisStore(url string) (*RedisStore, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	return &RedisStore{rdb: redis.NewClient(opts)}, nil
}

func (s *RedisStore) IncrByFloat(ctx context.Context, key string, delta float64) (float64, error) {
	return s.rdb.IncrByFloat(ctx, key, delta).Result()
}

func (s *RedisStore) Get(ctx context.Context, key string) (float64, error) {
	v, err := s.rdb.Get(ctx, key).Float64()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

func (s *RedisStore) Close() error {
	return s.rdb.Close()
}
