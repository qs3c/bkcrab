package goal

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed templates/*.md
var templatesFS embed.FS

var (
	continuationTmpl = mustParse("continuation.md")
	budgetLimitTmpl  = mustParse("budget_limit.md")
)

func mustParse(name string) *template.Template {
	body, err := templatesFS.ReadFile("templates/" + name)
	if err != nil {
		panic(fmt.Sprintf("goal: embedded template %s not found: %v", name, err))
	}
	t, err := template.New(name).Parse(string(body))
	if err != nil {
		panic(fmt.Sprintf("goal: template %s parse error: %v", name, err))
	}
	return t
}

// PromptVars 是传递给嵌入模板的视图。字段名称
// 匹配 templates/*.md 中的 {{ .X }} 引用。
type promptVars struct {
	Objective       string
	TokensUsed      int64
	TokenBudget     string // rendered as a string so we can show "none" / "unbounded"
	RemainingTokens string
}

func newPromptVars(g *Goal) promptVars {
	v := promptVars{
		Objective:  EscapeXMLText(g.Objective),
		TokensUsed: g.TokensUsed,
	}
	if g.TokenBudget == nil {
		v.TokenBudget = "none"
		v.RemainingTokens = "unbounded"
	} else {
		v.TokenBudget = fmt.Sprintf("%d", *g.TokenBudget)
		remaining, _ := RemainingTokens(g)
		v.RemainingTokens = fmt.Sprintf("%d", remaining)
	}
	return v
}

// ContinuationPrompt 呈现在注入时注入的每回合审核提示
// 目标是主动的。
func ContinuationPrompt(g *Goal) string {
	return render(continuationTmpl, newPromptVars(g))
}

// BudgetLimitPrompt 呈现在上注入一次的总结提示
// 将目标翻转为 BudgetLimited 的回合。
func BudgetLimitPrompt(g *Goal) string {
	return render(budgetLimitTmpl, newPromptVars(g))
}

func render(t *template.Template, v promptVars) string {
	var buf bytes.Buffer
	if err := t.Execute(&buf, v); err != nil {
		// 模板在初始化时嵌入并验证——渲染
		// 这里的错误意味着变量结构偏离了模板。
		panic(fmt.Sprintf("goal: %s render: %v", t.Name(), err))
	}
	return buf.String()
}

// EscapeXMLText 替换了三个字符，否则会使
// 用户提供的目标文本脱离 <objective> 包装器或
// 注入伪造的 </goal_context> 关闭标记。镜像 codex-rs/core/src/
// 目标.rs：1515-1520。
func EscapeXMLText(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
