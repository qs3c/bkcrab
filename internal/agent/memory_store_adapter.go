package agent

import (
	"context"

	"github.com/qs3c/bkclaw/internal/store"
)

// MemoryStoreAdapter 通过底层存储暴露代理的身份和内存文件。
// 读取传递 userID，使每个用户的覆盖行在存在时获胜（代理为该聊天者
// 自动持久化的 USER.md / MEMORY.md）；写入也携带 userID，使聊天时的
// 更新落在聊天者的行中，绝不会落入共享模板。
type MemoryStoreAdapter struct {
	st store.Store
}

func NewMemoryStoreAdapter(st store.Store) *MemoryStoreAdapter {
	return &MemoryStoreAdapter{st: st}
}

const memoryFilename = "MEMORY.md"

// GetMemory 故意使用 *Exact*（无所有者回退）变体。
// MEMORY.md 是每个聊天者的——公共链接访问者不得继承代理所有者积攒的
// 过去对话记忆。
func (a *MemoryStoreAdapter) GetMemory(ctx context.Context, agentID, userID string) (string, error) {
	data, err := a.st.GetAgentFileExact(ctx, agentID, userID, memoryFilename)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (a *MemoryStoreAdapter) SaveMemory(ctx context.Context, agentID, userID, content string) error {
	return a.st.SaveAgentFile(ctx, agentID, userID, memoryFilename, []byte(content))
}

// GetWorkspaceFile 保留所有者回退覆盖层，因为 ContextBuilder 对共享
// 身份文件（SOUL/IDENTITY/AGENTS/BOOTSTRAP/HEARTBEAT/TOOLS）使用此方法。
// 聊天者继承所有者的身份在那里是期望的行为。
func (a *MemoryStoreAdapter) GetWorkspaceFile(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	return a.st.GetAgentFile(ctx, agentID, userID, filename)
}

// GetWorkspaceFileExact 绕过所有者回退覆盖层。用于每个聊天者的文件
// （USER.md），使新访客看到空白的个人资料而不是所有者的。
func (a *MemoryStoreAdapter) GetWorkspaceFileExact(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	return a.st.GetAgentFileExact(ctx, agentID, userID, filename)
}

func (a *MemoryStoreAdapter) SaveWorkspaceFile(ctx context.Context, agentID, userID, filename string, data []byte) error {
	return a.st.SaveAgentFile(ctx, agentID, userID, filename, data)
}
