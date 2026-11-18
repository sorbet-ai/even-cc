package claude

import (
	"encoding/json"
	"maps"
	"time"
)

// UsageWindow is one plan limit window (five_hour, seven_day, ...).
type UsageWindow struct {
	Utilization float64 `json:"utilization"` // 0..1
	ResetsAt    int64   `json:"resetsAt"`    // unix seconds
	Status      string  `json:"status"`
}

// Usage is the account's plan usage, as the CLI last reported it in a
// rate_limit_event (only while this server runs a turn).
type Usage struct {
	Windows        map[string]UsageWindow `json:"windows"`
	IsUsingOverage bool                   `json:"isUsingOverage"`
	At             time.Time              `json:"at"`
}

// noteRateLimit merges a rate_limit_event's windows into the last usage.
func (m *Manager) noteRateLimit(line []byte) {
	var ev struct {
		Info struct {
			RateLimitType  string                 `json:"rateLimitType"`
			Status         string                 `json:"status"`
			ResetsAt       int64                  `json:"resetsAt"`
			Utilization    *float64               `json:"utilization"`
			IsUsingOverage bool                   `json:"isUsingOverage"`
			Windows        map[string]UsageWindow `json:"unifiedWindows"`
		} `json:"rate_limit_info"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	m.usageMu.Lock()
	defer m.usageMu.Unlock()
	u := Usage{Windows: map[string]UsageWindow{}, IsUsingOverage: ev.Info.IsUsingOverage, At: time.Now()}
	if m.usage != nil {
		maps.Copy(u.Windows, m.usage.Windows)
	}
	if t := ev.Info.RateLimitType; t != "" && ev.Info.Utilization != nil {
		u.Windows[t] = UsageWindow{Utilization: *ev.Info.Utilization, ResetsAt: ev.Info.ResetsAt, Status: ev.Info.Status}
	}
	maps.Copy(u.Windows, ev.Info.Windows)
	if len(u.Windows) == 0 {
		return
	}
	m.usage = &u
}

// Usage returns the last reported plan usage (nil before any).
func (m *Manager) Usage() *Usage {
	m.usageMu.Lock()
	defer m.usageMu.Unlock()
	if m.usage == nil {
		return nil
	}
	u := *m.usage
	u.Windows = maps.Clone(u.Windows)
	return &u
}
