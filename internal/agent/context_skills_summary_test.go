package agent

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
)

// TestBuildSystemPromptAsUsesPerCallSkillsSummary 确认技能摘要是按调用传入的
// 每回合值，而非共享字段。修复前 refreshSkillsFromStore 每回合写共享的
// a.ctxBuilder.skillsSummary，随后 BuildSystemPromptAs 读它——并发会话会互相
// 覆盖，导致系统提示里的技能清单串到别的会话。
func TestBuildSystemPromptAsUsesPerCallSkillsSummary(t *testing.T) {
	home := t.TempDir()
	cb := NewContextBuilder(home, NewMemory(home), "SHARED-DEFAULT")
	cb.SetPromptMode(config.PromptModeAgent)

	pA := cb.BuildSystemPromptAs("userA", NewMemory(home), "SKILLS-ALPHA")
	pB := cb.BuildSystemPromptAs("userB", NewMemory(home), "SKILLS-BRAVO")

	if !strings.Contains(pA, "SKILLS-ALPHA") {
		t.Errorf("A 的系统提示未包含自己的技能摘要")
	}
	if strings.Contains(pA, "SKILLS-BRAVO") {
		t.Errorf("A 的系统提示串入了 B 的技能摘要")
	}
	if !strings.Contains(pB, "SKILLS-BRAVO") {
		t.Errorf("B 的系统提示未包含自己的技能摘要")
	}
	if strings.Contains(pA, "SHARED-DEFAULT") {
		t.Errorf("传入的每回合摘要应覆盖构造期的共享默认值")
	}
}

// TestBuildSystemPromptAsConcurrentSkillsNoCrossTalk 并发下每个调用只应看到自己
// 传入的技能摘要（配合 go test -race 验证不再有共享写）。
func TestBuildSystemPromptAsConcurrentSkillsNoCrossTalk(t *testing.T) {
	home := t.TempDir()
	cb := NewContextBuilder(home, NewMemory(home), "")
	cb.SetPromptMode(config.PromptModeAgent)

	const n = 24
	var wg sync.WaitGroup
	errs := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := fmt.Sprintf("SKILLS-%d-END", i)
			p := cb.BuildSystemPromptAs(fmt.Sprintf("user-%d", i), NewMemory(home), marker)
			if !strings.Contains(p, marker) {
				errs[i] = fmt.Sprintf("prompt %d 缺自己的技能摘要 %s", i, marker)
			}
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != "" {
			t.Error(e)
		}
	}
}
