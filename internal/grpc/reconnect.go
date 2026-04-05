package grpc

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/kubexa/kubexa-agent/internal/config"
)

// ─────────────────────────────────────────
// Reconnector
// ─────────────────────────────────────────

// Reconnector will trigger a reconnect when the gRPC connection is lost.
// The reconnect will be triggered with an exponential backoff.
type Reconnector struct {
	cfg     config.GRPCConfig
	client  *Client
	trigger chan struct{}
	mu      sync.Mutex
	attempt int
}

func NewReconnector(cfg config.GRPCConfig, client *Client) *Reconnector {
	r := &Reconnector{
		cfg:     cfg,
		client:  client,
		trigger: make(chan struct{}, 1),
	}
	client.SetReconnector(r)
	return r
}

// ─────────────────────────────────────────
// Run
// ─────────────────────────────────────────

// Run starts the reconnect loop.
// It will run until the context is cancelled.
func (r *Reconnector) Run(ctx context.Context) {
	log.Info().Msg("reconnector başlatıldı")

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("reconnector durduruldu")
			return

		case <-r.trigger:
			r.reconnectWithBackoff(ctx)
		}
	}
}

// Trigger triggers a reconnect.
// If a reconnect is already in progress, the new trigger is ignored.
func (r *Reconnector) Trigger() {
	select {
	case r.trigger <- struct{}{}:
		log.Debug().Msg("reconnect triggered")
	default:
		// already triggered, waiting
	}
}

// ─────────────────────────────────────────
// Backoff
// ─────────────────────────────────────────

// reconnectWithBackoff reconnects with an exponential backoff.
// It will run until the context is cancelled.
func (r *Reconnector) reconnectWithBackoff(ctx context.Context) {
	r.mu.Lock()
	r.attempt = 0
	r.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		r.mu.Lock()
		attempt := r.attempt
		r.attempt++
		r.mu.Unlock()

		if r.cfg.MaxRetries > 0 && attempt >= r.cfg.MaxRetries {
			log.Error().
				Int("max_retries", r.cfg.MaxRetries).
				Msg("maximum retry count reached, reconnect stopped")
			return
		}

		delay := r.calcDelay(attempt)

		log.Info().
			Int("attempt", attempt+1).
			Int("max_retries", r.cfg.MaxRetries).
			Dur("delay", delay).
			Str("address", r.client.backendCfg.Address()).
			Msg("reconnecting...")

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		// mevcut bağlantıyı kapat
		if err := r.client.Close(); err != nil {
			log.Warn().Err(err).Msg("old connection could not be closed")
		}

		// yeniden bağlan
		if err := r.client.Connect(ctx); err != nil {
			log.Warn().
				Err(err).
				Int("attempt", attempt+1).
				Msg("reconnection failed")
			continue
		}

		log.Info().
			Int("attempt", attempt+1).
			Msg("reconnection successful")
		return
	}
}

// calcDelay exponential backoff + jitter calculates the delay.
// It calculates the delay using the exponential backoff formula.
// delay = min(base * 2^attempt, max) + jitter
// jitter = %20 random jitter (thundering herd prevention)
func (r *Reconnector) calcDelay(attempt int) time.Duration {
	base := r.cfg.RetryBaseDelay.Seconds()
	max := r.cfg.RetryMaxDelay.Seconds()

	// exponential: base * 2^attempt
	delay := base * math.Pow(2, float64(attempt))
	if delay > max {
		delay = max
	}

	// jitter: ±%20
	jitter := delay * 0.2 * (rand.Float64()*2 - 1) //nolint:gosec
	delay += jitter

	if delay < base {
		delay = base
	}

	return time.Duration(delay * float64(time.Second))
}
