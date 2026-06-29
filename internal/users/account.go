// Package users 拥有平台的身份层：真实用户账户（Account）和
// 他们签发的编程 token（APIKey）。两种类型都是 store.Store 上
// 的薄外观，因此单个 SQL 后端保持为跨 pod 的真实来源。
//
// 旧的"apikey == user"模型已不复存在。Account 是拥有代理/会话/
// cron 任务的实体；apikey 只是指向一个账户的范围化凭证，
// 带有可操作的代理的显式列表。
package users

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// 角色。super_admin 可以管理平台上的每个用户/代理/提供商；
// user 只能操作自己的资源。app_user 由 api_key 代表下游应用
// 进行配置——这些账户没有密码，无法通过仪表板或密码端点登录；
// 它们的存在纯粹是为了给外部最终用户一个稳定的 bkcrab user_id，
// 以便会话/agent_files/scope=user 配置按最终用户清晰分区。
// 有意不提供细粒度方案——任何更复杂的内容都存在于 apikey ACL 层。
const (
	RoleSuperAdmin = "super_admin"
	RoleUser       = "user"
	RoleAppUser    = "app_user"
)

// 状态。
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// ErrInvalidCredentials 掩盖"用户不存在"和"密码错误"，
// 使登录处理程序无法被用作邮箱存在性验证工具。
var ErrInvalidCredentials = errors.New("invalid credentials")

// Account 是用户行的公开表示。PasswordHash 从不离开此包——
// 我们在 Authenticate 期间读取它，并在返回给调用者之前将其清零。
type Account struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName,omitempty"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	APIKeyID    string    `json:"apikeyId,omitempty"`
	ExternalID  string    `json:"externalId,omitempty"`
	AvatarURL   string    `json:"avatarUrl,omitempty"`
	AgentQuota  int64     `json:"agentQuota"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Accounts 是用户账户的注册表。
type Accounts struct {
	store store.Store
}

// NewAccounts 返回由 st 支持的账户注册表。拒绝 nil——平台没有内存模式。
func NewAccounts(st store.Store) (*Accounts, error) {
	if st == nil {
		return nil, errors.New("users.NewAccounts: store is required")
	}
	return &Accounts{store: st}, nil
}

// Count 返回平台上的用户数量。入职流程通过 `Count(ctx) == 0`
// 来决定是否显示引导向导。
func (a *Accounts) Count(ctx context.Context) (int, error) {
	return a.store.CountUsers(ctx)
}

// CreateInput 是 Create 写入新用户行的一组字段。
// 必填：Username, Email, Password。Role 默认为 RoleUser。
//
// AgentQuota：
//   - nil           — 无限制（自注册用户的平台默认值）
//   - *value < 0    — 无限制
//   - *value = 0    — 调用者不能自行创建代理（仅管理员配置）
//   - *value > 0    — 调用者最多可拥有 N 个代理
//
// APIKeyID + ExternalID 是上游配置的幂等对。
// 将 APIKeyID 设置为正在创建此行的 apikey（处理程序从 auth.Identity
// 读取，而不是从请求体中读取），以便该行可审计回配置密钥。
// 将 ExternalID 设置为调用应用自身的用户标识符；
// (apikey_id, external_id) 上的部分 UNIQUE 索引——参见 migrateUsersAppUserCols
// ——意味着相同的对始终解析到相同的 bkcrab user_id，因此重试是安全的。
//
// AvatarURL 必须为空或 `data:image/*` URL ≤256KB；处理程序调用者负责该验证。
type CreateInput struct {
	Username    string
	Email       string
	Password    string
	DisplayName string
	Role        string
	AgentQuota  *int64
	AvatarURL   string
	APIKeyID    string
	ExternalID  string
}

// Create 写入一个新账户。密码使用 bcrypt 哈希；明文从不持久化。
// ID 始终自动生成。
//
// 对 (APIKeyID, ExternalID) 幂等：当两者都非空时，重复调用返回已配置的行，
// 而不是在部分 UNIQUE 索引上出错。上游应用可以重新发出相同的配置调用，
// 而无需跟踪是否之前调用过我们。跨*不同*身份的 username/email UNIQUE 冲突
// 仍然表现为错误——静默返回陌生人的行会隐藏真正的冲突。
func (a *Accounts) Create(ctx context.Context, in CreateInput) (*Account, error) {
	apikeyID := strings.TrimSpace(in.APIKeyID)
	externalID := strings.TrimSpace(in.ExternalID)
	// 快速路径——已为此 (apikey, external_id) 对配置。
	if apikeyID != "" && externalID != "" {
		if rec, err := a.store.GetUserByExternal(ctx, apikeyID, externalID); err == nil {
			return toAccount(rec), nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	username := strings.TrimSpace(in.Username)
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if username == "" || email == "" || in.Password == "" {
		return nil, errors.New("users.Create: username, email, password are required")
	}
	role := in.Role
	if role == "" {
		role = RoleUser
	}
	if role != RoleSuperAdmin && role != RoleUser {
		return nil, errors.New("users.Create: invalid role")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	id, err := newID("u_")
	if err != nil {
		return nil, err
	}
	quota := int64(-1)
	if in.AgentQuota != nil {
		quota = *in.AgentQuota
	}
	rec := &store.UserRecord{
		ID:           id,
		Username:     username,
		Email:        email,
		PasswordHash: string(hash),
		DisplayName:  in.DisplayName,
		Role:         role,
		Status:       StatusActive,
		APIKeyID:     apikeyID,
		ExternalID:   externalID,
		AvatarURL:    in.AvatarURL,
		AgentQuota:   quota,
	}
	if err := a.store.CreateUser(ctx, rec); err != nil {
		// 竞态：另一个并发请求在我们的快速路径未命中
		// 和 INSERT 之间创建了相同的 (apikey_id, external_id) 对。
		// 重新读取并返回该行，使调用者无论时序如何都能看到
		// 相同的幂等契约。跨不同身份的 username/email 冲突
		// 仍然会向上冒泡——参见 EnsureAppUser 的相同模式。
		if apikeyID != "" && externalID != "" {
			if again, qerr := a.store.GetUserByExternal(ctx, apikeyID, externalID); qerr == nil {
				return toAccount(again), nil
			}
		}
		return nil, err
	}
	return toAccount(rec), nil
}

// Authenticate 验证用户名或邮箱 + 密码对。成功时返回账户，
// 所有失败模式（用户不存在、密码错误、账户已禁用）都返回
// ErrInvalidCredentials，使调用者无法区分。
func (a *Accounts) Authenticate(ctx context.Context, login, password string) (*Account, error) {
	login = strings.TrimSpace(login)
	if login == "" || password == "" {
		return nil, ErrInvalidCredentials
	}
	if strings.Contains(login, "@") {
		login = strings.ToLower(login)
	}
	rec, err := a.store.GetUserByLogin(ctx, login)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if rec.Status != StatusActive {
		return nil, ErrInvalidCredentials
	}
	// app_user 账户（以及任何没有真实密码配置的行）带有空哈希。
	// bcrypt.CompareHashAndPassword 仍会失败关闭，但显式检查
	// 可使失败模式明确，并避免每次探测都消耗 bcrypt 计算资源。
	if rec.PasswordHash == "" || rec.Role == RoleAppUser {
		return nil, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(rec.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	return toAccount(rec), nil
}

// Get 返回 id 对应的账户，或 store.ErrNotFound。
func (a *Accounts) Get(ctx context.Context, id string) (*Account, error) {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	return toAccount(rec), nil
}

// List 返回所有账户。仅限超级管理员端点。
func (a *Accounts) List(ctx context.Context) ([]*Account, error) {
	recs, err := a.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Account, 0, len(recs))
	for i := range recs {
		out = append(out, toAccount(&recs[i]))
	}
	return out, nil
}

// Update 应用非凭证变更（显示名称、角色、状态）。
// 密码轮换请使用 SetPassword。
func (a *Accounts) Update(ctx context.Context, id, displayName, role, status string, agentQuota *int64) (*Account, error) {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	if displayName != "" {
		rec.DisplayName = displayName
	}
	if role != "" {
		if role != RoleSuperAdmin && role != RoleUser {
			return nil, errors.New("users.Update: invalid role")
		}
		rec.Role = role
	}
	if status != "" {
		if status != StatusActive && status != StatusDisabled {
			return nil, errors.New("users.Update: invalid status")
		}
		rec.Status = status
	}
	if agentQuota != nil {
		rec.AgentQuota = *agentQuota
	}
	if err := a.store.UpdateUser(ctx, rec); err != nil {
		return nil, err
	}
	return toAccount(rec), nil
}

// UpdateProfile 应用自助编辑——仅限显示名称和头像。
// 角色/状态变更通过 Update（仅管理员）。avatarURL 原样存储；
// 处理程序负责格式和大小验证。传入显式的空字符串可清除头像。
func (a *Accounts) UpdateProfile(ctx context.Context, id, displayName, avatarURL string) (*Account, error) {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	rec.DisplayName = displayName
	rec.AvatarURL = avatarURL
	if err := a.store.UpdateUser(ctx, rec); err != nil {
		return nil, err
	}
	return toAccount(rec), nil
}

// VerifyPassword 检查明文密码是否匹配 id 的存储哈希。
// 不匹配时（或对于没有密码的账户，如 app_user）返回
// ErrInvalidCredentials。由 /api/me/password 用于将自助密码变更
// 置于当前密码验证之后。
func (a *Accounts) VerifyPassword(ctx context.Context, id, password string) error {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return ErrInvalidCredentials
	}
	if rec.PasswordHash == "" {
		return ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(rec.PasswordHash), []byte(password)); err != nil {
		return ErrInvalidCredentials
	}
	return nil
}

// SetPassword 轮换账户的密码。调用者负责权限检查
//（自身 vs. super_admin）。
func (a *Accounts) SetPassword(ctx context.Context, id, newPassword string) error {
	if newPassword == "" {
		return errors.New("users.SetPassword: empty password")
	}
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	rec.PasswordHash = string(hash)
	return a.store.UpdateUser(ctx, rec)
}

// EnsureAppUser 返回代表 (apikeyID, externalID) 的 bkcrab 用户，
// 首次见到时创建一个 role=app_user 的用户。幂等：
// 后续使用相同对的调用返回现有行。调用者应为 api_key 所有者
// ——Mint 不进行认证，那是 auth 中间件的工作。Username/email
// 从该对合成并命名空间化（"ext:<apikeyID>:<externalID>"），
// 以便不与真人注册冲突，同时满足这些列上的 UNIQUE 约束。
func (a *Accounts) EnsureAppUser(ctx context.Context, apikeyID, externalID, displayName string) (*Account, error) {
	apikeyID = strings.TrimSpace(apikeyID)
	externalID = strings.TrimSpace(externalID)
	if apikeyID == "" || externalID == "" {
		return nil, errors.New("users.EnsureAppUser: apikeyID and externalID are required")
	}
	// 快速路径——已配置。
	if rec, err := a.store.GetUserByExternal(ctx, apikeyID, externalID); err == nil {
		return toAccount(rec), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	id, err := newID("u_")
	if err != nil {
		return nil, err
	}
	// 合成唯一的 username/email token。下游应用是人类可读身份
	// 的真实来源；我们只需要*某个*唯一值来满足 schema。
	syn := apikeyID + ":" + externalID
	rec := &store.UserRecord{
		ID:           id,
		Username:     "ext:" + syn,
		Email:        syn + "@external.bkcrab.local",
		PasswordHash: "",
		DisplayName:  displayName,
		Role:         RoleAppUser,
		Status:       StatusActive,
		APIKeyID:     apikeyID,
		ExternalID:   externalID,
		AgentQuota:   -1,
	}
	if err := a.store.CreateUser(ctx, rec); err != nil {
		// 竞态：另一个并发请求在我们的 GetUserByExternal 和 CreateUser
		// 之间创建了相同的对。重新读取并返回该行，而不是将唯一性违反
		// 向上冒泡给调用者。
		if again, qerr := a.store.GetUserByExternal(ctx, apikeyID, externalID); qerr == nil {
			return toAccount(again), nil
		}
		return nil, err
	}
	return toAccount(rec), nil
}

// Delete 删除账户及其拥有的行（级联在 store 中实现）。
// 拒绝删除最后一个 super_admin，以免安装将自己锁定。
func (a *Accounts) Delete(ctx context.Context, id string) error {
	target, err := a.store.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if target.Role == RoleSuperAdmin {
		all, err := a.store.ListUsers(ctx)
		if err != nil {
			return err
		}
		admins := 0
		for _, u := range all {
			if u.Role == RoleSuperAdmin && u.Status == StatusActive {
				admins++
			}
		}
		if admins <= 1 {
			return errors.New("users.Delete: refusing to remove the last active super_admin")
		}
	}
	return a.store.DeleteUser(ctx, id)
}

func toAccount(r *store.UserRecord) *Account {
	if r == nil {
		return nil
	}
	return &Account{
		ID:          r.ID,
		Username:    r.Username,
		Email:       r.Email,
		DisplayName: r.DisplayName,
		Role:        r.Role,
		Status:      r.Status,
		APIKeyID:    r.APIKeyID,
		ExternalID:  r.ExternalID,
		AvatarURL:   r.AvatarURL,
		AgentQuota:  r.AgentQuota,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

// newID 返回具有给定前缀的短唯一 ID。约 80 位熵——
// 在平台规模下碰撞概率极低。
func newID(prefix string) (string, error) {
	var buf [10]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf[:]), nil
}
