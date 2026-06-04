// Redis-backed rate-limit example: wires the RateLimit field with the
// distributed Redis backend (sliding-window-log algorithm + Lua-atomic
// allowance per ADR-005). A denied Allow surfaces as ErrRateLimited on
// the caller path so failover and pre-empt produce identically shaped
// errors.
//
// Run:
//
//	# Start a local Redis (docker, redis-server, etc.).
//	redis-server &
//	OPENAI_API_KEY=sk-... go run ./examples/ratelimit-redis
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	gateway "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider/openai"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/ratelimit"
)

func main() { os.Exit(run()) }

func run() int {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		slog.Error("OPENAI_API_KEY is required")
		return 2
	}

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = rdb.Close() }()

	// Sanity-check the connection up front — the rate-limit backend's
	// FailOpen contract would otherwise let a totally broken Redis pass
	// silently in this demo.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pingCancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		slog.Error("redis ping failed — start a local Redis or set REDIS_ADDR", "err", err)
		return 1
	}

	// Aggressive limits so the demo demonstrates the deny path on the
	// SECOND request. RPM=1 + TPM=10 means: first call succeeds, second
	// pre-empts with ErrRateLimited.
	limiter := ratelimit.NewRedis(rdb, ratelimit.Options{
		Limits: ratelimit.Limits{
			RequestsPerMinute: 1,
			TokensPerMinute:   10,
		},
	})

	gw, err := gateway.New(gateway.Config{
		Providers: []provider.Provider{openai.New(key)},
		RateLimit: limiter,
	})
	if err != nil {
		slog.Error("gateway init failed", "err", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// First request — succeeds within the RPM=1 budget.
	if err := doChat(ctx, gw, "req_rl_first"); err != nil {
		slog.Error("first chat failed", "err", err)
		return 1
	}

	// Second request — pre-empts with ErrRateLimited (RPM exhausted).
	err = doChat(ctx, gw, "req_rl_second")
	if !errors.Is(err, provider.ErrRateLimited) {
		slog.Error("expected ErrRateLimited on second call", "err", err)
		return 1
	}
	var pe *provider.ProviderError
	if errors.As(err, &pe) && pe.RetryAfter != nil {
		slog.Info("second call denied as expected", "retry_after", *pe.RetryAfter)
	} else {
		slog.Info("second call denied as expected (no RetryAfter hint)")
	}
	return 0
}

func doChat(ctx context.Context, gw *gateway.Gateway, requestID string) error {
	ctx = gateway.WithRequestID(ctx, requestID)
	maxTokens := 32
	resp, err := gw.Chat(ctx, provider.ChatRequest{
		Model:     "gpt-4o-mini",
		System:    "Reply in one short sentence.",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "Say hi."}},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		return err
	}
	fmt.Println(resp.Content)
	return nil
}
