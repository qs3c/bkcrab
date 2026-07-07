package agent

import (
	"testing"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
)

// TestManagerWiresSkillsLearnerInProductionPath 锁定发现 1:生产环境经
// NewManager → buildAgent(用 NewAgentWithSkillsCfg)构造 agent,此前从不
// 装配 skillsLearner,导致技能提炼与生命周期在生产静默失效。修复后,只要
// 通过 WithSkillsLearner 传入启用配置,生产路径构造的 agent 必须带 learner。
func TestManagerWiresSkillsLearnerInProductionPath(t *testing.T) {
	rc := config.ResolvedAgent{
		ID:        "agentX",
		Model:     "prov/m",
		Home:      t.TempDir(),
		Workspace: t.TempDir(),
	}
	mgr, err := NewManager([]config.ResolvedAgent{rc}, nil, bus.New(),
		WithUserID("u1"),
		WithSkillsLearner(config.SkillsLearnerCfg{MinToolCalls: 9}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ag := mgr.agents["agentX"]
	if ag == nil {
		t.Fatal("agent not built")
	}
	if ag.skillsLearner == nil {
		t.Fatal("production buildAgent must wire skills learner when enabled")
	}
	if ag.skillsLearner.agentID != "agentX" {
		t.Fatalf("learner agentID=%q want agentX", ag.skillsLearner.agentID)
	}
	if ag.skillsLearner.minToolCalls != 9 {
		t.Fatalf("learner minToolCalls=%d want 9", ag.skillsLearner.minToolCalls)
	}
}

// TestManagerSkipsLearnerWhenDisabled 确认显式关闭时生产路径不构造 learner。
func TestManagerSkipsLearnerWhenDisabled(t *testing.T) {
	no := false
	rc := config.ResolvedAgent{ID: "agentY", Model: "prov/m", Home: t.TempDir(), Workspace: t.TempDir()}
	mgr, err := NewManager([]config.ResolvedAgent{rc}, nil, bus.New(),
		WithUserID("u1"),
		WithSkillsLearner(config.SkillsLearnerCfg{Enabled: &no}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if ag := mgr.agents["agentY"]; ag == nil || ag.skillsLearner != nil {
		t.Fatalf("disabled learner must not be wired: %+v", ag)
	}
}
