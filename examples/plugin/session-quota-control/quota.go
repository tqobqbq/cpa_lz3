package main

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type quotaWindow struct {
	Name    string    `json:"name"`
	Used    float64   `json:"used_fraction"`
	ResetAt time.Time `json:"reset_at"`
}

type quotaPool struct {
	index           string
	authID          string
	provider        string
	windows         []quotaWindow
	observedAt      time.Time
	lastSeen        time.Time
	lastModel       string
	lastCost        float64
	lastDelay       time.Duration
	lastConcurrency float64
	reason          string
	history         concurrencyHistory
	learningWindows []quotaWindow
	learningAt      time.Time
	pendingModels   map[string]int
	seenUsage       map[string]struct{}
	usageOrder      []string
}

type activeRequest struct {
	pool     *quotaPool
	model    string
	readyAt  time.Time
	sleeping bool
}

type concurrencyPoint struct {
	at    time.Time
	count int
}
type concurrencyHistory struct {
	points  []concurrencyPoint
	current int
}

func (history *concurrencyHistory) prune(now time.Time) {
	// Retain one point before the longest configurable lookback boundary.
	cutoff := now.Add(-time.Hour)
	first := 0
	for first+1 < len(history.points) && !history.points[first+1].at.After(cutoff) {
		first++
	}
	if first > 0 {
		history.points = append([]concurrencyPoint(nil), history.points[first:]...)
	}
}

func (history *concurrencyHistory) change(now time.Time, delta int) {
	history.prune(now)
	history.current = max(0, history.current+delta)
	if len(history.points) > 0 && history.points[len(history.points)-1].at.Equal(now) {
		history.points[len(history.points)-1].count = history.current
	} else {
		history.points = append(history.points, concurrencyPoint{at: now, count: history.current})
	}
}

func (history *concurrencyHistory) average(now time.Time, window time.Duration) float64 {
	history.prune(now)
	start := now.Add(-window)
	var area float64
	for index, point := range history.points {
		end := now
		if index+1 < len(history.points) {
			end = history.points[index+1].at
		}
		if point.at.After(start) {
			start = point.at
		}
		if end.After(start) {
			area += float64(point.count) * end.Sub(start).Seconds()
			start = end
		}
	}
	// No recent measured load is treated as one request, never as zero demand.
	return max(1, area/window.Seconds())
}

type modelAverage struct{ samples []float64 }

func (average *modelAverage) add(cost float64, count, limit int) {
	count = min(count, limit)
	for range count {
		average.samples = append(average.samples, cost)
	}
	if len(average.samples) > limit {
		average.samples = append([]float64(nil), average.samples[len(average.samples)-limit:]...)
	}
}

func (average *modelAverage) mean(limit int) float64 {
	values := average.samples[max(0, len(average.samples)-limit):]
	var sum float64
	for _, sample := range values {
		sum += sample
	}
	if len(values) == 0 {
		return 0
	}
	return sum / float64(len(values))
}

func modelName(model string) string { return thinking.ParseSuffix(strings.TrimSpace(model)).ModelName }
func modelKey(provider, model string) string {
	return strings.ToLower(provider) + "\x00" + modelName(model)
}

func finiteNumber(raw string) (float64, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, false
	}
	value, errParse := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	return value, errParse == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func quotaTime(raw string) (time.Time, bool) {
	if number, ok := finiteNumber(raw); ok && number > 0 && number < 1e12 {
		return time.Unix(int64(number), int64((number-math.Floor(number))*1e9)), true
	}
	parsed, errParse := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	return parsed, errParse == nil
}

func readWindows(provider string, signals map[string]string, observedAt time.Time) []quotaWindow {
	if observedAt.IsZero() {
		return nil
	}
	headers := make(map[string]string, len(signals))
	for name, value := range signals {
		headers[strings.ToLower(name)] = value
	}
	var windows []quotaWindow
	switch strings.ToLower(provider) {
	case "claude":
		used, okUsed := finiteNumber(headers["anthropic-ratelimit-unified-5h-utilization"])
		reset, okReset := quotaTime(headers["anthropic-ratelimit-unified-5h-reset"])
		if strings.EqualFold(headers["anthropic-ratelimit-unified-5h-status"], "rejected") {
			used, okUsed = 1, true
		}
		if okUsed && used >= 0 && okReset {
			windows = append(windows, quotaWindow{Name: "5h", Used: min(used, 1), ResetAt: reset})
		}
	case "codex":
		for name, value := range headers {
			if !strings.HasPrefix(name, "x-codex-") || !strings.HasSuffix(name, "-window-minutes") {
				continue
			}
			minutes, valid := finiteNumber(value)
			if !valid || minutes != 300 {
				continue
			}
			prefix := strings.TrimSuffix(name, "window-minutes")
			used, okUsed := finiteNumber(headers[prefix+"used-percent"])
			reset, okReset := quotaTime(headers[prefix+"reset-at"])
			if !okReset {
				if seconds, okSeconds := finiteNumber(headers[prefix+"reset-after-seconds"]); okSeconds && seconds >= 0 && seconds <= 86400 {
					reset, okReset = observedAt.Add(time.Duration(seconds*float64(time.Second))), true
				}
			}
			if okUsed && used >= 0 && okReset {
				windows = append(windows, quotaWindow{Name: strings.TrimSuffix(prefix, "-"), Used: min(used/100, 1), ResetAt: reset})
			}
		}
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].Name < windows[j].Name })
	return windows
}

func (c *control) poolLocked(index, authID, provider string, now time.Time) *quotaPool {
	pool := c.pools[index]
	if pool == nil {
		pool = &quotaPool{index: index, pendingModels: make(map[string]int), seenUsage: make(map[string]struct{})}
		c.pools[index] = pool
	}
	if authID != "" {
		pool.authID = authID
	}
	if provider != "" {
		pool.provider = provider
	}
	pool.lastSeen = now
	return pool
}

func (c *control) modelCostLocked(provider, model string, cfg config) float64 {
	if average := c.models[modelKey(provider, model)]; average != nil {
		if cost := average.mean(cfg.ModelSamples); cost > 0 {
			return cost
		}
	}
	return cfg.InitialCost / 100
}

func fixedDelay(windows []quotaWindow, now time.Time, cost, concurrency, margin float64) (time.Duration, string) {
	var delay time.Duration
	found := false
	for _, window := range windows {
		if !window.ResetAt.After(now) {
			continue
		}
		found = true
		horizon := min(window.ResetAt.Sub(now), 24*time.Hour)
		remaining := 1 - window.Used - margin/100
		share := 1.0
		if remaining > 0 {
			share = min(1, cost*concurrency/remaining)
		}
		delay = max(delay, time.Duration(float64(horizon)*share))
	}
	if !found {
		return 0, "no current five-hour observation"
	}
	return delay, "fixed estimate"
}

func (c *control) pace(req interceptCall) pluginapi.RequestInterceptResponse {
	selectionID := c.reportSelection(req)
	index, _ := req.Metadata[coreexecutor.SelectedAuthIndexMetadataKey].(string)
	authID, _ := req.Metadata[coreexecutor.SelectedAuthMetadataKey].(string)
	if index == "" || req.RequestID == "" {
		return pluginapi.RequestInterceptResponse{}
	}
	c.mu.Lock()
	cfg, closed := c.cfg, c.closed
	rule := cfg.credentialWait(index, authID)
	c.mu.Unlock()
	if closed || !cfg.QuotaEnabled {
		return pluginapi.RequestInterceptResponse{}
	}
	provider := req.ToFormat
	var snapshot pluginapi.HostAuthQuotaResponse
	var windows []quotaWindow
	if rule.FixedWaitSeconds == nil {
		// This is the only quota lookup for this credential selection. The fixed
		// override does not read quota or evaluate the automatic formula at all.
		var errQuery error
		snapshot, errQuery = c.quota(index)
		if errQuery != nil {
			return terminated(http.StatusServiceUnavailable, "cannot read selected credential quota")
		}
		if snapshot.Disabled {
			return terminated(http.StatusServiceUnavailable, "selected credential is disabled")
		}
		provider, authID = snapshot.Provider, snapshot.AuthID
		if provider != "claude" && provider != "codex" {
			return pluginapi.RequestInterceptResponse{}
		}
		windows = readWindows(provider, snapshot.Signals, snapshot.ObservedAt)
	}
	c.mu.Lock()
	now := c.now()
	c.finishLocked(req.RequestID, now)
	pool := c.poolLocked(index, authID, provider, now)
	pool.windows, pool.observedAt, pool.lastModel = windows, snapshot.ObservedAt, modelName(req.Model)
	var delay time.Duration
	var cost, concurrency float64
	reason := "manual fixed delay"
	if rule.FixedWaitSeconds != nil {
		delay = time.Duration(*rule.FixedWaitSeconds) * time.Second
	} else {
		cost = c.modelCostLocked(provider, req.Model, cfg)
		concurrency = pool.history.average(now, time.Duration(cfg.ConcurrencyWindow)*time.Second)
		delay, reason = fixedDelay(windows, now, cost, concurrency, cfg.SafetyMargin)
		if len(pool.learningWindows) == 0 && len(windows) > 0 {
			pool.learningWindows, pool.learningAt = windows, snapshot.ObservedAt
		}
	}
	if rule.MaxWaitSeconds != nil && delay > time.Duration(*rule.MaxWaitSeconds)*time.Second {
		delay = time.Duration(*rule.MaxWaitSeconds) * time.Second
		reason += "; capped"
	}
	pool.lastCost, pool.lastDelay, pool.lastConcurrency, pool.reason = cost, delay, concurrency, reason
	if delay > 0 && c.waiting+len(c.pending) >= cfg.MaxPending {
		c.mu.Unlock()
		return terminated(http.StatusTooManyRequests, "too many sleeping requests")
	}
	active := &activeRequest{pool: pool, model: modelName(req.Model), readyAt: now.Add(delay), sleeping: delay > 0}
	c.active[req.RequestID] = active
	pool.history.change(now, 1)
	if active.sleeping {
		c.waiting++
	}
	c.mu.Unlock()
	c.reportQuotaPlan(req.RequestID, selectionID, delay, reason, cost, concurrency, windows, snapshot.ObservedAt)
	if delay <= 0 {
		return pluginapi.RequestInterceptResponse{}
	}
	// One timer waits for the duration decided above. Only downstream cancellation
	// or plugin unload can interrupt it; later quota/configuration changes do not.
	waitStartedAt := c.now()
	errWait := c.wait(req.CallbackID, delay)
	waitFinishedAt := c.now()
	c.reportQuotaEnd(req.RequestID, selectionID, waitStartedAt, waitFinishedAt)
	c.mu.Lock()
	if current := c.active[req.RequestID]; current == active {
		if errWait != nil {
			c.finishLocked(req.RequestID, c.now())
		} else if active.sleeping {
			active.sleeping = false
			c.waiting--
		}
	}
	c.mu.Unlock()
	if errWait != nil {
		return terminated(499, "request canceled during fixed quota wait")
	}
	return pluginapi.RequestInterceptResponse{}
}

func (c *control) finishLocked(requestID string, completedAt time.Time) {
	active := c.active[requestID]
	if active == nil {
		return
	}
	delete(c.active, requestID)
	if active.sleeping {
		c.waiting--
	}
	if completedAt.IsZero() {
		completedAt = c.now()
	}
	// Terminal callbacks run asynchronously; history transitions use processing
	// time when an older timestamp would otherwise reverse their order.
	points := active.pool.history.points
	if len(points) > 0 && completedAt.Before(points[len(points)-1].at) {
		completedAt = c.now()
	}
	active.pool.history.change(completedAt, -1)
	active.pool.lastSeen = c.now()
}

func (c *control) recordUsage(record pluginapi.UsageRecord) {
	if record.Provider != "claude" && record.Provider != "codex" {
		return
	}
	index := record.AuthIndex
	if index == "" {
		index = record.AuthID
	}
	if index == "" || record.Model == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pool := c.poolLocked(index, record.AuthID, record.Provider, c.now())
	if record.RequestID != "" {
		if _, seen := pool.seenUsage[record.RequestID]; seen {
			return
		}
		pool.seenUsage[record.RequestID] = struct{}{}
		pool.usageOrder = append(pool.usageOrder, record.RequestID)
		if len(pool.usageOrder) > 4096 {
			delete(pool.seenUsage, pool.usageOrder[0])
			pool.usageOrder = pool.usageOrder[1:]
		}
	}
	observedAt := record.RequestedAt.Add(record.Latency)
	if record.RequestedAt.IsZero() {
		observedAt = c.now()
	}
	if !pool.learningAt.IsZero() && !observedAt.After(pool.learningAt) {
		return
	}
	key := modelKey(record.Provider, record.Model)
	pool.pendingModels[key]++
	signals := make(map[string]string, len(record.ResponseHeaders))
	for name, values := range record.ResponseHeaders {
		if len(values) > 0 {
			signals[name] = values[len(values)-1]
		}
	}
	windows := readWindows(record.Provider, signals, observedAt)
	if len(windows) == 0 {
		return
	}
	var delta float64
	matched := false
	for _, current := range windows {
		for _, previous := range pool.learningWindows {
			if current.Name != previous.Name || math.Abs(current.ResetAt.Sub(previous.ResetAt).Seconds()) >= 60 {
				continue
			}
			matched = true
			delta = max(delta, current.Used-previous.Used)
		}
	}
	if matched && delta <= 0 {
		// Rounded percentages may stay unchanged across several completions.
		// Keep their request counts until a positive change can be measured.
		pool.learningAt = observedAt
		return
	}
	if matched && len(pool.pendingModels) == 1 {
		// A credential snapshot cannot separate simultaneous consumption by
		// different models. Learn only from intervals attributable to one model.
		count := pool.pendingModels[key]
		if count > 0 {
			average := c.models[key]
			if average == nil {
				average = &modelAverage{}
				c.models[key] = average
			}
			average.add(delta/float64(count), count, c.cfg.ModelSamples)
		}
	}
	pool.learningWindows, pool.learningAt = windows, observedAt
	clear(pool.pendingModels)
}
