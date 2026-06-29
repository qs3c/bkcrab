package goal

import (
	"context"

	"github.com/qs3c/bkcrab/internal/store"
)

// 当 (agent,
// session_key) 对已经有一个目标。与底层别名
// 存储错误，以便调用者编写errors.Is(err, goal.ErrAlreadyExists)
// 捕获数据库层的本机错误和任何包本地错误
// 速记。
var ErrAlreadyExists = store.ErrGoalAlreadyExists

// 当不存在目标时 GetGoalBySession 返回 ErrNotFound
// 请求的（代理、会话密钥）。别名为存储错误
// 与 ErrAlreadyExists 的原因相同。
var ErrNotFound = store.ErrNotFound

// Store 是斜线处理程序的狭义持久化接口，
// 延续助手和会计挂钩依赖。这是一个子集
// 商店。商店；生产代码电线存储。直接存储
// （DBStore 隐式地满足了这一点）——该接口的存在是为了保持
// 可使用内存中的假测试进行测试。
type Store interface {
	CreateGoal(ctx context.Context, g *Goal) error
	GetGoalBySession(ctx context.Context, agentID, sessionKey string) (*Goal, error)
	UpdateGoal(ctx context.Context, g *Goal) error
	DeleteGoal(ctx context.Context, goalID string) error
}
