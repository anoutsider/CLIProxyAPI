package cliproxy

import (
	"context"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// claudePoolSampler stores each Claude account's weekly windows every
// coreauth.PoolSampleInterval, the history the pool forecast reads.
type claudePoolSampler struct {
	service *Service
	history *coreauth.PoolHistory
	last    time.Time
}

func newClaudePoolSampler(service *Service, history *coreauth.PoolHistory) *claudePoolSampler {
	return &claudePoolSampler{service: service, history: history}
}

func (p *claudePoolSampler) run(ctx context.Context) {
	ticker := time.NewTicker(claudeScopedUsageTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			p.tick(ctx, now)
		}
	}
}

// tick samples once per interval, at the first tick of each interval.
func (p *claudePoolSampler) tick(ctx context.Context, now time.Time) {
	slot := now.UTC().Truncate(coreauth.PoolSampleInterval)
	if !slot.After(p.last) || p.service == nil || p.service.coreManager == nil {
		return
	}
	p.last = slot
	samples := coreauth.ClaudePoolSamples(p.service.coreManager.List(), now)
	if len(samples) == 0 {
		return
	}
	if errAppend := p.history.Append(ctx, samples, now); errAppend != nil {
		log.Warnf("claude pool sampler: %v", errAppend)
	}
}
