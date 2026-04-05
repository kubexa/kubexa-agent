package agent

import (
	"fmt"

	"github.com/rs/zerolog/log"

	"github.com/kubexa/kubexa-agent/internal/buffer"
	"github.com/kubexa/kubexa-agent/internal/config"
)

// buildOutboundSpillovers creates pod and log spillover buffers (memory + optional Redis).
// cleanup must be called on shutdown.
func buildOutboundSpillovers(cfg *config.Config) (pod *buffer.SpilloverBuffer, logBuf *buffer.SpilloverBuffer, cleanup func(), err error) {
	capacity := cfg.Buffer.Memory.Capacity
	if capacity <= 0 {
		capacity = cfg.Logs.BufferSize
	}
	if capacity < 64 {
		capacity = 64
	}

	memCfg := buffer.MemoryConfig{
		Capacity:       capacity,
		MaxAttempts:    cfg.Buffer.Memory.MaxAttempts,
		DeadLetterSize: 100,
	}
	if memCfg.MaxAttempts <= 0 {
		memCfg.MaxAttempts = 5
	}

	memPod, err := buffer.NewMemoryBuffer(memCfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pod memory buffer: %w", err)
	}
	memLog, err := buffer.NewMemoryBuffer(memCfg)
	if err != nil {
		_ = memPod.Close()
		return nil, nil, nil, fmt.Errorf("log memory buffer: %w", err)
	}

	var rPod, rLog *buffer.RedisBuffer
	if cfg.Buffer.Redis.Enabled {
		rc := cfg.Buffer.Redis
		addr := rc.Address()
		cfgPod := buffer.DefaultRedisBufferConfig(addr, fmt.Sprintf("kubexa:%s:pod", cfg.ClusterID))
		cfgPod.Password = rc.Password
		cfgPod.DB = rc.DB

		var e error
		rPod, e = buffer.NewRedisBuffer(cfgPod)
		if e != nil {
			log.Warn().Err(e).Msg("redis pod spillover unavailable; memory-only for pod outbound")
			rPod = nil
		}

		cfgLog := buffer.DefaultRedisBufferConfig(addr, fmt.Sprintf("kubexa:%s:log", cfg.ClusterID))
		cfgLog.Password = rc.Password
		cfgLog.DB = rc.DB
		rLog, e = buffer.NewRedisBuffer(cfgLog)
		if e != nil {
			log.Warn().Err(e).Msg("redis log spillover unavailable; memory-only for log outbound")
			rLog = nil
		}
	}

	spPod := buffer.NewSpilloverBuffer(memPod, rPod, capacity)
	spLog := buffer.NewSpilloverBuffer(memLog, rLog, capacity)

	cleanup = func() {
		if e := spPod.Close(); e != nil {
			log.Warn().Err(e).Msg("pod spillover close")
		}
		if e := spLog.Close(); e != nil {
			log.Warn().Err(e).Msg("log spillover close")
		}
	}

	return spPod, spLog, cleanup, nil
}
