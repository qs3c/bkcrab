package users

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/qs3c/bkcrab/internal/store"
)

// APIKey 类型层级。在创建时设置在 apikey 上；不可变。
//
//   - APIKeyTypeAdmin：平台级权限——只有 super_admin 所有者才能签发此类型。
//     绕过代理 ACL 并解锁 /api/admin/* 路由。
//   - APIKeyTypeUser：限定到拥有用户的资源。可以创建代理（代理随后属于所有者）
//     并访问所有者已拥有的任何代理。此层级忽略 apikey_agents。
//   - APIKeyTypeAgent：锁定到显式的代理列表（apikey_agents）。
//     不能创建代理。用于"给下游应用授予 N 个特定代理的密钥"的场景。
const (
	APIKeyTypeAdmin = "admin"
	APIKeyTypeUser  = "user"
	APIKeyTypeAgent = "agent"
)

// IsAPIKeyType 报告 s 是否为规范的层级字符串之一。
func IsAPIKeyType(s string) bool {
	return s == APIKeyTypeAdmin || s == APIKeyTypeUser || s == APIKeyTypeAgent
}

// APIKey 是 apikey 行的公开表示。Key 在列表响应中保存
// 掩码显示字符串（"fc_xxxx****"），在创建/轮换时保存
// 新签发的明文 token。哈希值从不返回。
type APIKey struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	Name      string    `json:"name,omitempty"`
	Key       string    `json:"key"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"createdAt"`
}

// Resolved 是 auth 中间件授权请求所需的内容：apikey、
// 其拥有者用户以及此密钥可操作的代理。通过 LookupByToken
// 一次性获取，使热路径保持单次往返。
//
// 对于 type=user 的密钥，Agents 在解析时填充 apikey 所有者拥有的
// 所有代理（请求中途创建的新代理直到下次请求才会出现）。
// 对于 type=agent，它是显式的 ACL 列表。对于 type=admin，它为空——认证门基于类型短路。
type Resolved struct {
	APIKey  APIKey
	Account Account
	Agents  []string
}

// APIKeys 是编程凭证的注册表。
type APIKeys struct {
	store store.Store
}

// NewAPIKeys 返回由 st 支持的 apikey 注册表。
func NewAPIKeys(st store.Store) (*APIKeys, error) {
	if st == nil {
		return nil, errors.New("users.NewAPIKeys: store is required")
	}
	return &APIKeys{store: st}, nil
}

// Create 为 userID 签发一个新的 apikey。明文 token 返回一次且不可恢复。
//
// keyType 必须是 APIKeyTypeAdmin/User/Agent 之一。agentIDs 仅对
// type=agent 生效（否则忽略——user/admin 层级从所有者的所有权派生其范围，
// 而不是从显式列表）。对于 type=agent，列表必须非空；
// 一个无法访问任何代理的 agent 层级密钥将无法使用。
// 调用者负责角色与类型的策略检查（handlers_admin.go 强制执行
// "只有 super_admin 才能签发 type=admin"等）。
func (k *APIKeys) Create(ctx context.Context, userID, name, keyType string, agentIDs []string) (*APIKey, string, error) {
	if userID == "" {
		return nil, "", errors.New("users.APIKeys.Create: userID is required")
	}
	if keyType == "" {
		keyType = APIKeyTypeAgent
	}
	if !IsAPIKeyType(keyType) {
		return nil, "", errors.New("users.APIKeys.Create: invalid type (want admin|user|agent)")
	}
	if keyType == APIKeyTypeAgent && len(agentIDs) == 0 {
		return nil, "", errors.New("users.APIKeys.Create: type=agent requires at least one agent")
	}
	id, err := newID("k_")
	if err != nil {
		return nil, "", err
	}
	token, err := newToken()
	if err != nil {
		return nil, "", err
	}
	rec := &store.APIKeyRecord{
		ID:        id,
		UserID:    userID,
		Name:      name,
		KeyHash:   hashToken(token),
		KeyPrefix: keyPrefix(token),
		Type:      keyType,
		CreatedAt: time.Now().UTC(),
	}
	if err := k.store.CreateAPIKey(ctx, rec); err != nil {
		return nil, "", err
	}
	if keyType == APIKeyTypeAgent {
		if err := k.store.SetAPIKeyAgents(ctx, id, agentIDs); err != nil {
			return nil, "", err
		}
	}
	out := toAPIKey(rec)
	out.Key = token
	return out, token, nil
}

// Rotate 替换 apikey 的 token。旧 token 立即失效。
func (k *APIKeys) Rotate(ctx context.Context, id string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	if err := k.store.RotateAPIKey(ctx, id, hashToken(token), keyPrefix(token)); err != nil {
		return "", err
	}
	return token, nil
}

func (k *APIKeys) Delete(ctx context.Context, id string) error {
	return k.store.DeleteAPIKey(ctx, id)
}

func (k *APIKeys) Get(ctx context.Context, id string) (*APIKey, error) {
	rec, err := k.store.GetAPIKey(ctx, id)
	if err != nil {
		return nil, err
	}
	return toAPIKey(rec), nil
}

// List 返回 userID 拥有的每个 apikey，Key 字段已掩码。
func (k *APIKeys) List(ctx context.Context, userID string) ([]*APIKey, error) {
	recs, err := k.store.ListAPIKeys(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]*APIKey, 0, len(recs))
	for i := range recs {
		out = append(out, toAPIKey(&recs[i]))
	}
	return out, nil
}

// Agents 返回 apikey 的代理访问列表。
func (k *APIKeys) Agents(ctx context.Context, apikeyID string) ([]string, error) {
	return k.store.ListAPIKeyAgents(ctx, apikeyID)
}

// SetAgents 替换 apikey 的代理访问列表。仅对 type=agent 有意义——
// admin/user 层级从所有者（而非 apikey_agents）派生范围，
// 因此编辑那里的列表在认证时会静默无操作。拒绝这些调用，
// 以免调用者将"设置成功"误认为"范围已更改"。
func (k *APIKeys) SetAgents(ctx context.Context, apikeyID string, agentIDs []string) error {
	rec, err := k.store.GetAPIKey(ctx, apikeyID)
	if err != nil {
		return err
	}
	if rec.Type != "" && rec.Type != APIKeyTypeAgent {
		return errors.New("users.APIKeys.SetAgents: agent list is only editable on type=agent keys")
	}
	if len(agentIDs) == 0 {
		return errors.New("users.APIKeys.SetAgents: at least one agent required")
	}
	return k.store.SetAPIKeyAgents(ctx, apikeyID, agentIDs)
}

// LookupByToken 是认证热路径。SHA256(token) → (apikey, account,
// access list)。任何失败模式都返回 ErrInvalidCredentials，
// 使中间件无法区分"未知"和"已禁用"。
//
// 对于 type=agent，我们读取显式的 apikey_agents ACL。对于 type=user，
// 我们替换为所有者的完整代理列表——新创建的代理在下次请求时自动包含，
// 无需任何 ACL 维护。对于 type=admin，我们完全跳过列表；
// 代理门在查阅它之前基于类型短路。
func (k *APIKeys) LookupByToken(ctx context.Context, token string) (*Resolved, error) {
	if token == "" {
		return nil, ErrInvalidCredentials
	}
	rec, err := k.store.LookupAPIKeyByHash(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	user, err := k.store.GetUser(ctx, rec.UserID)
	if err != nil {
		// 孤儿 apikey（用户已删除但 apikey 残留）。视为无效——
		// 级联本应捕获这种情况。
		return nil, ErrInvalidCredentials
	}
	if user.Status != StatusActive {
		return nil, ErrInvalidCredentials
	}
	var agents []string
	switch rec.Type {
	case APIKeyTypeAdmin:
		// Admin 密钥完全绕过按代理的门控；留空。
	case APIKeyTypeUser:
		// apikey 所有者拥有的所有代理。每次请求第二个列表是
		// "新代理无需 ACL 维护"的代价。
		ags, err := k.store.ListAgents(ctx, rec.UserID)
		if err != nil {
			return nil, err
		}
		agents = make([]string, 0, len(ags))
		for _, a := range ags {
			agents = append(agents, a.ID)
		}
	default:
		// type=agent（以及任何遗留/未知值）→ 显式 ACL。
		agents, err = k.store.ListAPIKeyAgents(ctx, rec.ID)
		if err != nil {
			return nil, err
		}
	}
	return &Resolved{
		APIKey:  *toAPIKey(rec),
		Account: *toAccount(user),
		Agents:  agents,
	}, nil
}

// CanAccessAgent 回答"此 apikey 是否可以操作 agentID？"
func (k *APIKeys) CanAccessAgent(ctx context.Context, apikeyID, agentID string) (bool, error) {
	return k.store.APIKeyCanAccessAgent(ctx, apikeyID, agentID)
}

func toAPIKey(rec *store.APIKeyRecord) *APIKey {
	if rec == nil {
		return nil
	}
	masked := rec.KeyPrefix
	if masked == "" {
		masked = "fc_********"
	} else {
		masked = masked + "****"
	}
	return &APIKey{
		ID:        rec.ID,
		UserID:    rec.UserID,
		Name:      rec.Name,
		Key:       masked,
		Type:      rec.Type,
		CreatedAt: rec.CreatedAt,
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// keyPrefix 保留明文的一段可识别切片用于 UI 显示。
// 10 个字符足以在列表中识别出"你的"密钥，同时远低于暴力破解的可行性。
func keyPrefix(token string) string {
	if len(token) <= 10 {
		return token
	}
	return token[:10]
}

func newToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "fc_" + hex.EncodeToString(buf[:]), nil
}
