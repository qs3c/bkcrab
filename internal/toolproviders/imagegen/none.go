package imagegen

import (
	"context"

	"github.com/qs3c/bkclaw/internal/toolproviders"
)

// None 是一个标记提供商，表示"不要向模型暴露 image_gen"。
// 工具注册层（internal/agent/tools/image_gen.go）检测到链中有 "none" 时
// 会完全跳过注册 image_gen，以便模型回退到其原生图像生成能力（或没有）。
//
// 它选择加入 CredentialFree，以便当 "none" 是唯一配置的提供商时，
// chain.Available() 返回 true——仪表盘可以区分"管理员做了明确选择"
// 和"忘记配置任何东西"。
type None struct{}

func (None) Category() string     { return Category }
func (None) Name() string         { return "none" }
func (None) CredentialFree() bool { return true }

// Execute 不应被执行：image_gen 注册在链运行前就会因 "none" 而短路。
// 该错误是防御性的——如果有人以新的方式连接链并绕过了跳过逻辑，
// 应该大声地暴露失败，而不是静默地返回空结果。
func (None) Execute(_ context.Context, _ toolproviders.Request) (toolproviders.Response, error) {
	return toolproviders.Response{}, toolproviders.ErrNoResults
}
