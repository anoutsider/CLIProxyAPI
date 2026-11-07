package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
)

// PoolHistoryRetention is how long pool samples are kept.
const PoolHistoryRetention = 30 * 24 * time.Hour

// PoolSampleInterval is the spacing of pool samples.
const PoolSampleInterval = 10 * time.Minute

const poolHistoryKeyPrefix = "pool-forecast:samples:"

// PoolSampleInPool is the PoolSample state of an account the pool counts.
const PoolSampleInPool = "in-pool"

// PoolSample is one Claude account at a sample time, with its weekly windows as
// last observed. A window whose reset has passed is stored as observed; the
// forecast counts it as reset when it reads the sample.
type PoolSample struct {
	At    time.Time `json:"at"`
	Email string    `json:"email"`
	// State is PoolSampleInPool, or the forecast state that leaves the account
	// out of the pool: PoolStateDisabled or PoolStateNeedsSignIn.
	State string `json:"state"`
	// SevenDay is the seven-day window, or nil when it was never observed.
	SevenDay *PoolSampleWindow `json:"sevenDay,omitempty"`
	// Fable is Fable's weekly window, or nil when it was not read.
	Fable *PoolSampleWindow `json:"fable,omitempty"`
}

// PoolSampleWindow is a weekly window as the hub observed it.
type PoolSampleWindow struct {
	// Used is the fraction of the window used, from 0 to 1.
	Used       float64   `json:"used"`
	ResetsAt   time.Time `json:"resetsAt"`
	ObservedAt time.Time `json:"observedAt"`
}

func poolSampleWindowOf(window ScopedWindow) *PoolSampleWindow {
	return &PoolSampleWindow{Used: window.Utilization, ResetsAt: window.ResetsAt.UTC(), ObservedAt: window.ObservedAt.UTC()}
}

func (w *PoolSampleWindow) utc() *PoolSampleWindow {
	if w == nil {
		return nil
	}
	return &PoolSampleWindow{Used: w.Used, ResetsAt: w.ResetsAt.UTC(), ObservedAt: w.ObservedAt.UTC()}
}

// PoolHistory stores pool samples in the hub's key-value store, one key per UTC
// day, each kept for PoolHistoryRetention after its day ends. Without a
// key-value store the samples are kept in memory.
type PoolHistory struct {
	// client is replaced in tests; nil means the current kvstore client.
	client func() (kvstore.KVClient, bool, error)

	mu     sync.Mutex
	memory map[string][]PoolSample
}

var defaultPoolHistory = &PoolHistory{}

// DefaultPoolHistory returns the process-wide history backed by the current
// key-value store, shared by the sampler and the forecast endpoint.
func DefaultPoolHistory() *PoolHistory {
	return defaultPoolHistory
}

func (h *PoolHistory) kv() (kvstore.KVClient, error) {
	resolve := kvstore.CurrentKVClient
	if h.client != nil {
		resolve = h.client
	}
	client, enabled, errClient := resolve()
	if errClient != nil {
		return nil, errClient
	}
	if !enabled || client == nil {
		return nil, nil
	}
	return client, nil
}

func poolHistoryKey(day time.Time) string {
	return poolHistoryKeyPrefix + day.UTC().Format("2006-01-02")
}

// Append adds samples, replacing any earlier sample with the same email and time,
// and drops samples older than PoolHistoryRetention.
func (h *PoolHistory) Append(ctx context.Context, samples []PoolSample, now time.Time) error {
	byDay := make(map[time.Time][]PoolSample)
	for _, sample := range samples {
		sample.At = sample.At.UTC()
		sample.SevenDay = sample.SevenDay.utc()
		sample.Fable = sample.Fable.utc()
		sample.Email = strings.TrimSpace(sample.Email)
		if sample.Email == "" {
			continue
		}
		day := sample.At.Truncate(24 * time.Hour)
		byDay[day] = append(byDay[day], sample)
	}
	client, errKV := h.kv()
	if errKV != nil {
		return errKV
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for day, added := range byDay {
		ttl := day.Add(24 * time.Hour).Add(PoolHistoryRetention).Sub(now)
		if ttl <= 0 {
			continue
		}
		key := poolHistoryKey(day)
		existing, errLoad := h.loadKey(ctx, client, key)
		if errLoad != nil {
			return errLoad
		}
		merged := mergePoolSamples(existing, added)
		if client == nil {
			if h.memory == nil {
				h.memory = make(map[string][]PoolSample)
			}
			h.memory[key] = merged
			continue
		}
		raw, errMarshal := json.Marshal(merged)
		if errMarshal != nil {
			return errMarshal
		}
		if _, errSet := client.KVSet(ctx, key, raw, kvstore.KVSetOptions{EX: ttl}); errSet != nil {
			return fmt.Errorf("pool history: write %s: %w", key, errSet)
		}
	}
	h.pruneMemory(now)
	return nil
}

func (h *PoolHistory) loadKey(ctx context.Context, client kvstore.KVClient, key string) ([]PoolSample, error) {
	if client == nil {
		return append([]PoolSample(nil), h.memory[key]...), nil
	}
	raw, ok, errGet := client.KVGet(ctx, key)
	if errGet != nil {
		return nil, fmt.Errorf("pool history: read %s: %w", key, errGet)
	}
	if !ok {
		return nil, nil
	}
	var samples []PoolSample
	if errDecode := json.Unmarshal(raw, &samples); errDecode != nil {
		return nil, fmt.Errorf("pool history: decode %s: %w", key, errDecode)
	}
	return samples, nil
}

func (h *PoolHistory) pruneMemory(now time.Time) {
	for key := range h.memory {
		day, errParse := time.Parse("2006-01-02", strings.TrimPrefix(key, poolHistoryKeyPrefix))
		if errParse != nil || day.Add(24*time.Hour).Add(PoolHistoryRetention).Before(now) {
			delete(h.memory, key)
		}
	}
}

// Load returns the samples of the last PoolHistoryRetention, oldest first.
func (h *PoolHistory) Load(ctx context.Context, now time.Time) ([]PoolSample, error) {
	client, errKV := h.kv()
	if errKV != nil {
		return nil, errKV
	}
	from := now.UTC().Add(-PoolHistoryRetention)
	var keys []string
	for day := from.Truncate(24 * time.Hour); !day.After(now); day = day.Add(24 * time.Hour) {
		keys = append(keys, poolHistoryKey(day))
	}
	var samples []PoolSample
	h.mu.Lock()
	defer h.mu.Unlock()
	if client == nil {
		for _, key := range keys {
			samples = append(samples, h.memory[key]...)
		}
	} else {
		values, found, errGet := client.KVMGet(ctx, keys...)
		if errGet != nil {
			return nil, fmt.Errorf("pool history: read: %w", errGet)
		}
		for i, raw := range values {
			if i >= len(found) || !found[i] {
				continue
			}
			var day []PoolSample
			if errDecode := json.Unmarshal(raw, &day); errDecode != nil {
				return nil, fmt.Errorf("pool history: decode %s: %w", keys[i], errDecode)
			}
			samples = append(samples, day...)
		}
	}
	kept := samples[:0]
	for _, sample := range samples {
		if !sample.At.Before(from) && !sample.At.After(now) {
			kept = append(kept, sample)
		}
	}
	sortPoolSamples(kept)
	return kept, nil
}

func mergePoolSamples(existing, added []PoolSample) []PoolSample {
	type sampleKey struct {
		email string
		at    int64
	}
	byKey := make(map[sampleKey]PoolSample, len(existing)+len(added))
	for _, sample := range existing {
		byKey[sampleKey{sample.Email, sample.At.Unix()}] = sample
	}
	for _, sample := range added {
		byKey[sampleKey{sample.Email, sample.At.Unix()}] = sample
	}
	merged := make([]PoolSample, 0, len(byKey))
	for _, sample := range byKey {
		merged = append(merged, sample)
	}
	sortPoolSamples(merged)
	return merged
}

func sortPoolSamples(samples []PoolSample) {
	sort.SliceStable(samples, func(i, j int) bool {
		if !samples[i].At.Equal(samples[j].At) {
			return samples[i].At.Before(samples[j].At)
		}
		return samples[i].Email < samples[j].Email
	})
}
