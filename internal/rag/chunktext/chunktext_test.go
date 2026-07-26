package chunktext

import "testing"

func TestSearchAndBodyRoundTrip(t *testing.T) {
	t.Parallel()
	search := Search("安装指南 > Linux", "运行安装命令。")
	if search != "章节：安装指南 > Linux\n\n运行安装命令。" {
		t.Fatalf("search content = %q", search)
	}
	if got := Body(search, "安装指南 > Linux"); got != "运行安装命令。" {
		t.Fatalf("body = %q", got)
	}
}

func TestBodySupportsOldRowsAndTruncatedBreadcrumbs(t *testing.T) {
	t.Parallel()
	old := "旧索引只有正文。"
	if got := Body(old, "章节"); got != old {
		t.Fatalf("old row changed to %q", got)
	}
	truncated := Search("…Linux > Docker", "compose up")
	if got := Body(truncated, "安装 > Linux > Docker"); got != "compose up" {
		t.Fatalf("truncated breadcrumb body = %q", got)
	}
	collision := "章节：正文中的普通标签\n\n后续内容"
	if got := Body(collision, "另一个章节"); got != collision {
		t.Fatalf("unrelated content envelope was stripped: %q", got)
	}
}

func TestAnswerKeepsEnhancementSubordinate(t *testing.T) {
	t.Parallel()
	if got := Answer("原始表格", "这是表格摘要"); got != "原始表格\n\n语义辅助（可能有误，原文优先）：\n这是表格摘要" {
		t.Fatalf("answer = %q", got)
	}
	if got := Answer("  原文  ", ""); got != "原文" {
		t.Fatalf("raw-only answer = %q", got)
	}
	if got := AppendEnhancement("章节：标题\n\n原文", "摘要"); got != "章节：标题\n\n原文\n\n语义辅助（可能有误，原文优先）：\n摘要" {
		t.Fatalf("enhanced search = %q", got)
	}
}
