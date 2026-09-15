package config

import "testing"

func TestMetricsEnv(t *testing.T) {
	for _, tc := range []struct {
		enabled, addr string
		valid         bool
	}{
		{"", "", true}, {"true", ":18954", true}, {"true", "127.0.0.1:18954", true},
		{"true", "[::1]:18954", true}, {"false", "bad", true}, {"bad", "", false},
		{"true", "bad", false}, {"true", ":0", false}, {"true", ":65536", false},
	} {
		t.Run(tc.enabled+tc.addr, func(t *testing.T) {
			t.Setenv("BKCRAB_METRICS_ENABLED", tc.enabled)
			t.Setenv("BKCRAB_METRICS_ADDR", tc.addr)
			c := LoadEnv().Metrics
			if (c.Validate() == nil) != tc.valid {
				t.Fatal(c.Validate())
			}
			if tc.enabled == "" && (c.Enabled || c.Addr != "127.0.0.1:18954") {
				t.Fatal(c)
			}
		})
	}
}
