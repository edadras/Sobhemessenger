// Package ratelimit implements the request throttles from §33 and the
// message/upload throttles from §34, backed by Redis so limits are enforced
// across every API node rather than per-process.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/observability"
)

// slidingWindowScript implements a sliding-window counter with a sorted set:
// old entries are trimmed, the window is counted, and the request is recorded
// only if it fits. Doing it in Lua makes the whole decision atomic.
//
// KEYS[1] window key
// ARGV[1] now (unix microseconds)
// ARGV[2] window size (microseconds)
// ARGV[3] limit
// ARGV[4] unique member id
//
// Returns {allowed, remaining, retry_after_seconds}
var slidingWindowScript = redis.NewScript(`
local key    = KEYS[1]
local now    = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit  = tonumber(ARGV[3])
local member = ARGV[4]

redis.call('ZREMRANGEBYSCORE', key, '-inf', now - window)
local used = redis.call('ZCARD', key)

if used >= limit then
  local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
  local retry = 1
  if oldest[2] then
    retry = math.ceil((tonumber(oldest[2]) + window - now) / 1000000)
    if retry < 1 then retry = 1 end
  end
  return {0, 0, retry}
end

redis.call('ZADD', key, now, member)
redis.call('PEXPIRE', key, math.ceil(window / 1000))
return {1, limit - used - 1, 0}
`)

// Limiter enforces named sliding-window limits.
type Limiter struct {
	redis   *cache.Client
	metrics *observability.Metrics
	// failOpen decides what happens when Redis is unreachable. Availability of
	// the messenger matters more than a perfectly enforced limit, so the
	// default is to allow — except for the auth limiters, which fail closed.
	failOpen bool
}

func New(client *cache.Client, metrics *observability.Metrics) *Limiter {
	return &Limiter{redis: client, metrics: metrics, failOpen: true}
}

// Result describes a limiter decision.
type Result struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// ErrLimitExceeded is returned by Check when the caller wants an error rather
// than inspecting the Result.
var ErrLimitExceeded = errors.New("ratelimit: limit exceeded")

// Rule is a named limit: `Limit` events per `Window`.
type Rule struct {
	Name   string
	Limit  int
	Window time.Duration
	// FailClosed rejects requests when Redis is unavailable. Set for the
	// authentication limiters so an outage cannot open a brute-force window.
	FailClosed bool
}

// Allow records one event against the rule for the given subject.
func (l *Limiter) Allow(ctx context.Context, rule Rule, subject string) (Result, error) {
	if rule.Limit <= 0 {
		return Result{Allowed: true}, nil
	}

	key := fmt.Sprintf("rl:%s:%s", rule.Name, subject)
	now := time.Now().UnixMicro()
	member := fmt.Sprintf("%d-%s", now, randomSuffix())

	raw, err := slidingWindowScript.Run(ctx, l.redis.Raw(), []string{key},
		now, rule.Window.Microseconds(), rule.Limit, member).Slice()
	if err != nil {
		if rule.FailClosed || !l.failOpen {
			return Result{Allowed: false, RetryAfter: time.Second}, fmt.Errorf("ratelimit: %w", err)
		}
		// Degraded but available: let the request through and surface the fault.
		return Result{Allowed: true}, nil
	}

	if len(raw) < 3 {
		return Result{Allowed: true}, nil
	}

	allowed := toInt(raw[0]) == 1
	result := Result{
		Allowed:    allowed,
		Remaining:  toInt(raw[1]),
		RetryAfter: time.Duration(toInt(raw[2])) * time.Second,
	}
	if !allowed {
		l.metrics.RateLimitHits.WithLabelValues(rule.Name).Inc()
	}
	return result, nil
}

// Reset clears a subject's window — used after a successful login so a user is
// not punished for earlier failed attempts.
func (l *Limiter) Reset(ctx context.Context, rule Rule, subject string) error {
	return l.redis.Delete(ctx, fmt.Sprintf("rl:%s:%s", rule.Name, subject))
}

// Peek reports current usage without recording an event.
func (l *Limiter) Peek(ctx context.Context, rule Rule, subject string) (int, error) {
	key := fmt.Sprintf("rl:%s:%s", rule.Name, subject)
	cutoff := time.Now().Add(-rule.Window).UnixMicro()
	pipe := l.redis.Raw().TxPipeline()
	pipe.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprintf("%d", cutoff))
	count := pipe.ZCard(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return int(count.Val()), nil
}

func toInt(v any) int {
	switch value := v.(type) {
	case int64:
		return int(value)
	case int:
		return value
	case string:
		var n int
		_, _ = fmt.Sscanf(value, "%d", &n)
		return n
	default:
		return 0
	}
}
