package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrNotPreferred = errors.New("узел уступил лидерство узлу с более высоким приоритетом")

type Lease struct {
	client             *redis.Client
	key, value, nodeID string
	priority           int
	ttl, heartbeat     time.Duration
}

func New(rawURL, key, nodeID string, priority int, ttl, heartbeat time.Duration) (*Lease, error) {
	o, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	c := redis.NewClient(o)
	if err := c.Ping(context.Background()).Err(); err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &Lease{client: c, key: key, value: hex.EncodeToString(b), nodeID: nodeID, priority: priority, ttl: ttl, heartbeat: heartbeat}, nil
}

func (l *Lease) Register(ctx context.Context) error {
	return l.client.Set(ctx, l.candidateKey(l.nodeID), strconv.Itoa(l.priority), 3*l.ttl).Err()
}

func (l *Lease) preferred(ctx context.Context) (bool, error) {
	keys, err := l.client.Keys(ctx, l.key+":candidate:*").Result()
	if err != nil {
		return false, err
	}
	bestID, bestPriority := "", int(^uint(0)>>1)
	for _, key := range keys {
		id := strings.TrimPrefix(key, l.key+":candidate:")
		value, err := l.client.Get(ctx, key).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			return false, err
		}
		priority, err := strconv.Atoi(value)
		if err != nil {
			continue
		}
		if priority < bestPriority || (priority == bestPriority && id < bestID) {
			bestPriority, bestID = priority, id
		}
	}
	return bestID == l.nodeID, nil
}

func (l *Lease) candidateKey(id string) string { return l.key + ":candidate:" + id }

func (l *Lease) Acquire(ctx context.Context) (bool, error) {
	if err := l.Register(ctx); err != nil {
		return false, err
	}
	preferred, err := l.preferred(ctx)
	if err != nil || !preferred {
		return false, err
	}
	return l.client.SetNX(ctx, l.key, l.value, l.ttl).Result()
}

func (l *Lease) Renew(ctx context.Context) error {
	if err := l.Register(ctx); err != nil {
		return err
	}
	preferred, err := l.preferred(ctx)
	if err != nil {
		return err
	}
	if !preferred {
		return ErrNotPreferred
	}
	n, err := l.client.Eval(ctx, `if redis.call('get',KEYS[1]) == ARGV[1] then return redis.call('pexpire',KEYS[1],ARGV[2]) else return 0 end`, []string{l.key}, l.value, l.ttl.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("лидерская аренда потеряна")
	}
	return nil
}

func (l *Lease) Release(ctx context.Context) error {
	_, err := l.client.Eval(ctx, `if redis.call('get',KEYS[1]) == ARGV[1] then return redis.call('del',KEYS[1]) else return 0 end`, []string{l.key}, l.value).Result()
	return err
}

func (l *Lease) ClaimOnce(ctx context.Context, key string) (bool, error) {
	return l.client.SetNX(ctx, l.key+":"+key, "sent", 48*time.Hour).Result()
}
func (l *Lease) Close() error { return l.client.Close() }
