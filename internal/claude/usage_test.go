package claude

import "testing"

func TestUsageFromRateLimitEvents(t *testing.T) {
	h := newHarness(t, Display{})
	if h.m.Usage() != nil {
		t.Fatal("usage before any event")
	}
	h.m.noteRateLimit([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1793356800,"rateLimitType":"five_hour","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.21,"resetsAt":1793356800,"status":"allowed"},"seven_day":{"utilization":0.24,"resetsAt":1793754000,"status":"allowed"}}}}`))
	// a later event naming one window updates it and keeps the other
	h.m.noteRateLimit([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1793356800,"rateLimitType":"five_hour","utilization":0.8}}`))
	u := h.m.Usage()
	if u == nil || u.Windows["five_hour"].Utilization != 0.8 || u.Windows["seven_day"].Utilization != 0.24 {
		t.Fatalf("%+v", u)
	}
	h.m.noteRateLimit([]byte(`not json`))
	if h.m.Usage().Windows["five_hour"].Status != "allowed_warning" {
		t.Error("bad line changed usage")
	}
}
