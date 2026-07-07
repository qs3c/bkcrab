package config

import "testing"

func TestSkillsLearnerCfgIsEnabled(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		in   *bool
		want bool
	}{
		{"nil defaults enabled", nil, true},
		{"explicit true enabled", &yes, true},
		{"explicit false disabled", &no, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := SkillsLearnerCfg{Enabled: tc.in}
			if got := c.IsEnabled(); got != tc.want {
				t.Fatalf("IsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSkillLifecycleCfgIsEnabled(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		in   *bool
		want bool
	}{
		{"nil defaults enabled", nil, true},
		{"explicit true enabled", &yes, true},
		{"explicit false disabled", &no, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := SkillLifecycleCfg{Enabled: tc.in}
			if got := c.IsEnabled(); got != tc.want {
				t.Fatalf("IsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}
