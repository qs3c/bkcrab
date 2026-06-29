// Package auth 将 HTTP 请求解析为用户身份。它支持两种凭证类型：
//   - cookie 会话 ("bkcrab_session")：由 /api/login 设置，基于 web_sessions 表验证；供 Web UI 使用
//   - Bearer apikey：基于 apikeys 表验证；供 API 消费者和 CLI 客户端使用
//
// 两条路径最终汇聚为同一个 Identity 结构体，通过 config.WithUserID 印入 ctx。
// 不存在匿名的"local"回退——没有有效凭证的请求将收到 401。
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

// SessionCookieName 是支撑 Web UI 登录状态的 cookie 名称。
const SessionCookieName = "bkcrab_session"

// SessionTTL 是新签发的登录 cookie 的有效时长。
const SessionTTL = 30 * 24 * time.Hour

// Identity 是针对单个请求解析后的调用方身份。
type Identity struct {
	UserID string
	Role   string

	// AuthMethod 取值为 "session" 或 "apikey"。
	AuthMethod string

	// APIKeyID 在 AuthMethod=="apikey" 时设置。APIKeyType 是
	// users.APIKeyType{Admin,User,Agent} 中的一种。APIKeyAgents 是在
	// 认证时解析的 agent 范围：
	//   - type=admin：空（agent 门控按类型短路）
	//   - type=user：请求时刻 apikey 拥有者拥有的所有 agent（每次请求重新解析）
	//   - type=agent：来自 apikey_agents 的显式 ACL
	APIKeyID     string
	APIKeyType   string
	APIKeyAgents []string

	// ActAsUserID 在 super_admin 通过 ?actAs= 以只读方式浏览其他
	// 用户资源时非空。设置此字段时，变更加载器必须返回 403。
	ActAsUserID string
}

// EffectiveUserID 返回我们为其读取数据的用户。对于 actAs 模式下的 super_admin，
// 返回被模拟的用户；否则返回调用方自身。
func (i Identity) EffectiveUserID() string {
	if i.ActAsUserID != "" {
		return i.ActAsUserID
	}
	return i.UserID
}

// IsActingAs 报告 super_admin 是否正在模拟另一个用户。
func (i Identity) IsActingAs() bool {
	return i.ActAsUserID != "" && i.ActAsUserID != i.UserID
}

// ReadOnly 报告变更加载点是否必须拒绝此请求。
// 处于 actAs 模式是此处强制执行的唯一只读条件。
func (i Identity) ReadOnly() bool {
	return i.IsActingAs()
}

// CanAccessAgent 回答"此调用方是否有权访问 agentID？"
//   - super_admin（session）：是，可访问任何 agent（actAs 时只读）
//   - apikey type=admin：是，可访问任何 agent
//   - apikey type=user/agent：仅当 agentID ∈ APIKeyAgents（该列表
//     在认证时按类型预解析——参见 Resolved.Agents）
//   - session 用户（非 admin）：agent 必须属于 UserID（由调用方
//     查询 agents 表验证；Identity 上不携带该列表）
func (i Identity) CanAccessAgent(agentID string) bool {
	if i.AuthMethod == "apikey" {
		if i.APIKeyType == users.APIKeyTypeAdmin {
			return true
		}
		for _, a := range i.APIKeyAgents {
			if a == agentID {
				return true
			}
		}
		return false
	}
	if i.Role == users.RoleSuperAdmin {
		return true
	}
	// session 调用方：agent 归属检查在处理程序中读取 agent 行后进行
	//（轻量 M:1 查找，无需扫描列表）。
	return true
}

// CanAdminPlatform 回答"此调用方是否可以访问 /api/admin/* 及其他
// 平台级变更加载点？"仅 super_admin session 和 type=admin apikey
// 符合条件。与 CanAccessAgent 不同，因为 super_admin 的 type=user/agent
// apikey 会故意将其降级到签发时指定的较窄范围。
func (i Identity) CanAdminPlatform() bool {
	if i.AuthMethod == "apikey" {
		return i.APIKeyType == users.APIKeyTypeAdmin
	}
	return i.Role == users.RoleSuperAdmin
}

// CanCreateAgent 回答"此调用方是否可以创建新 agent？"
// type=agent 密钥明确不能——它们被沙箱限制在固定列表中。
// 其他所有调用方（session 以及 admin/user 密钥）都可以。
func (i Identity) CanCreateAgent() bool {
	if i.AuthMethod == "apikey" && i.APIKeyType == users.APIKeyTypeAgent {
		return false
	}
	return true
}

type identityKey struct{}

// WithIdentity 将解析后的身份印入 ctx，以便处理程序无需重新验证凭证即可读取。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	ctx = context.WithValue(ctx, identityKey{}, id)
	if uid := id.EffectiveUserID(); uid != "" {
		ctx = config.WithUserID(ctx, uid)
	}
	return ctx
}

// FromContext 返回 Middleware 印入的解析后身份。如果未执行认证，
// bool 值为 false，这意味着路由配置错误（每个 API 路由都必须先经过 Middleware）。
func FromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	v, ok := ctx.Value(identityKey{}).(Identity)
	return v, ok
}

// Resolver 从存储中加载账户、apikey 和 web session。
type Resolver struct {
	store    store.Store
	apikeys  *users.APIKeys
	accounts *users.Accounts
}

// NewResolver 返回一个绑定到平台存储的解析器。
func NewResolver(st store.Store) (*Resolver, error) {
	if st == nil {
		return nil, errors.New("auth.NewResolver: store is required")
	}
	ak, err := users.NewAPIKeys(st)
	if err != nil {
		return nil, err
	}
	ac, err := users.NewAccounts(st)
	if err != nil {
		return nil, err
	}
	return &Resolver{store: st, apikeys: ak, accounts: ac}, nil
}

// IssueSession 为 userID 创建 web session 并返回 cookie。
// 调用方将 cookie 写入响应。
func (r *Resolver) IssueSession(ctx context.Context, userID string) (*http.Cookie, error) {
	sid, err := newSID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rec := &store.WebSessionRecord{
		SID:       sid,
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(SessionTTL),
	}
	if err := r.store.CreateWebSession(ctx, rec); err != nil {
		return nil, err
	}
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  rec.ExpiresAt,
	}, nil
}

// RevokeSession 从存储中删除一个 session。
func (r *Resolver) RevokeSession(ctx context.Context, sid string) error {
	return r.store.DeleteWebSession(ctx, sid)
}

// ResolveSession 将 cookie SID 转换为 Identity。
func (r *Resolver) ResolveSession(ctx context.Context, sid string) (Identity, error) {
	if sid == "" {
		return Identity{}, ErrUnauthorized
	}
	sess, err := r.store.GetWebSession(ctx, sid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Identity{}, ErrUnauthorized
		}
		return Identity{}, err
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		_ = r.store.DeleteWebSession(ctx, sid)
		return Identity{}, ErrUnauthorized
	}
	user, err := r.accounts.Get(ctx, sess.UserID)
	if err != nil {
		return Identity{}, ErrUnauthorized
	}
	if user.Status != users.StatusActive {
		return Identity{}, ErrUnauthorized
	}
	return Identity{
		UserID:     user.ID,
		Role:       user.Role,
		AuthMethod: "session",
	}, nil
}

// ResolveBearer 将 Bearer token 转换为 Identity。
func (r *Resolver) ResolveBearer(ctx context.Context, token string) (Identity, error) {
	res, err := r.apikeys.LookupByToken(ctx, token)
	if err != nil {
		if errors.Is(err, users.ErrInvalidCredentials) {
			return Identity{}, ErrUnauthorized
		}
		return Identity{}, err
	}
	return Identity{
		UserID:       res.Account.ID,
		Role:         res.Account.Role,
		AuthMethod:   "apikey",
		APIKeyID:     res.APIKey.ID,
		APIKeyType:   res.APIKey.Type,
		APIKeyAgents: append([]string(nil), res.Agents...),
	}, nil
}

// SwitchToAppUser 将 ident 重新绑定到与 (ident.APIKeyID, externalID)
// 关联的 app_user，首次遇到时创建该行。保留 APIKeyID + APIKeyAgents——
// 仅 UserID 和 Role 切换——因此 apikey 的 agent ACL 仍然控制访问。
// 空的 externalID 原样传递。仅对 AuthMethod=="apikey" 有效；
// session 调用方保持不变。
func (r *Resolver) SwitchToAppUser(ctx context.Context, ident Identity, externalID string) (Identity, error) {
	if externalID == "" {
		return ident, nil
	}
	if ident.AuthMethod != "apikey" || ident.APIKeyID == "" {
		return ident, errors.New("auth.SwitchToAppUser: api_key auth required")
	}
	acc, err := r.accounts.EnsureAppUser(ctx, ident.APIKeyID, externalID, "")
	if err != nil {
		return ident, err
	}
	ident.UserID = acc.ID
	ident.Role = acc.Role
	return ident, nil
}

// EndUserHeader 是每个请求的头部，指明调用应用的终端用户。
// 当在 api_key 认证的请求上设置此头部时，auth 中间件会延迟创建（或查找）
// 一个 (apikey, header) 对应的 bkcrab 用户，并将请求身份切换为该用户。
// 在该身份下写入的 Session 和 agent_files 将按终端用户清晰隔离，
// 而不是堆积在 api_key 拥有者名下。
const EndUserHeader = "X-Bkcrab-End-User"

// ErrUnauthorized 在没有有效凭证时返回。
var ErrUnauthorized = errors.New("unauthorized")

// extract 从请求中返回 bearer token（如果有）和 session cookie SID（如果有）。
// 也接受 `?token=` 查询参数，但仅限确实需要它的狭窄路径集合——文件下载
// （浏览器在通过 <img> / <a download> 渲染时无法添加 Authorization 头部）
// 和聊天 SSE 订阅（EventSource 没有头部 API）。其他所有地方都拒绝查询参数
// 回退：URL 中的 token 会通过 Referer、浏览器历史、反向代理访问日志和可观测
// 性管道泄漏。在 /v1/* 和 /api/* 其余路径上仅强制使用头部，堵住了泄漏面；
// 之前为这些端点构建 `?token=` URL 的 CLI 脚本必须切换到
// `Authorization: Bearer <token>`（每个 HTTP 客户端都支持）。
func extract(r *http.Request) (bearer, sid string) {
	if c, err := r.Cookie(SessionCookieName); err == nil {
		sid = c.Value
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if t := strings.TrimPrefix(h, "Bearer "); t != h {
			bearer = t
		}
	} else if t := r.URL.Query().Get("token"); t != "" && queryTokenAllowed(r) {
		bearer = t
	}
	return bearer, sid
}

// queryTokenAllowed 将 `?token=` bearer 回退限制到一个狭窄的路径
// 白名单，这些路径的客户端没有其他方式附加 Authorization 头部。
//
// 允许的路径：
//   - GET /api/agents/<id>/files/...        — workspace 文件下载
//   - GET /api/agents/<id>/files.zip        — workspace 归档
//   - GET /api/agents/<id>/system-files/<n> — 身份文件获取（少见）
//   - GET /api/chat/subscribe               — EventSource SSE 流
//
// 其他所有路径（/v1/*、/api/chat、/api/agents/<id> JSON 等）必须
// 使用 Authorization 头部。有意不进行 /api/agents/<id>/files 的前缀
// 匹配，因为该前缀下某些 workspace 端点接受 POST/PUT 请求体——限制为
// GET 以确保写入路径永远无法通过被记录的 URL 进行认证。
func queryTokenAllowed(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/api/agents/") && strings.Contains(p, "/files"):
		return true
	case strings.HasPrefix(p, "/api/agents/") && strings.Contains(p, "/system-files/"):
		return true
	case p == "/api/chat/subscribe":
		return true
	}
	return false
}

// Middleware 在每个包装的路由上强制执行认证。无凭证或凭证无效时返回 401。
// 为 super_admin 解析 ?actAs=。
func (r *Resolver) Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ident, err := r.resolve(req)
		if err != nil {
			writeUnauthorized(w)
			return
		}
		req = req.WithContext(WithIdentity(req.Context(), ident))
		next(w, req)
	}
}

// Optional 在凭证存在时进行解析，但允许未经认证的请求通过。
// 用于引导期间的 /api/status，以便引导 UI 在任何用户存在之前探测安装状态。
func (r *Resolver) Optional(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ident, err := r.resolve(req)
		if err == nil {
			req = req.WithContext(WithIdentity(req.Context(), ident))
		}
		next(w, req)
	}
}

func (r *Resolver) resolve(req *http.Request) (Identity, error) {
	bearer, sid := extract(req)

	var ident Identity
	var err error
	if sid != "" {
		ident, err = r.ResolveSession(req.Context(), sid)
		if err == nil {
			goto done
		}
	}
	if bearer != "" {
		ident, err = r.ResolveBearer(req.Context(), bearer)
		if err == nil {
			goto done
		}
	}
	return Identity{}, ErrUnauthorized

done:
	// actAs 保留给 super_admin，仅适用于 session 调用方
	//（apikey 模拟会破坏 apikey ACL）。
	if act := req.URL.Query().Get("actAs"); act != "" {
		if ident.AuthMethod == "session" && ident.Role == users.RoleSuperAdmin {
			ident.ActAsUserID = act
		}
	}
	// 如果调用应用在 api_key 请求上通过 X-Bkcrab-End-User 指定了终端用户，
	// 则重新绑定到对应的 app_user（延迟创建）。这里我们吞掉错误，以使格式错误的
	// 头部不会导致请求返回 401——请求仍保持在 api_key 拥有者名下。OpenAI
	// /v1/chat/completions 处理程序也会为偏好 OpenAI 格式的客户端处理请求体中的
	// `user` 字段；该路径在解析请求体后显式调用 SwitchToAppUser。
	if eu := strings.TrimSpace(req.Header.Get(EndUserHeader)); eu != "" {
		if next, swErr := r.SwitchToAppUser(req.Context(), ident, eu); swErr == nil {
			ident = next
		}
	}
	return ident, nil
}

// RequireSuperAdmin 返回一个中间件，对任何非 super_admin 的调用方返回 403。
// 包装另一个中间件（通常是 auth Middleware）。
//
// 这是最严格的网关：要求实时调用方的身份为 super_admin，无论他们如何认证。
// 使用 type=user apikey 的 super_admin 会被拒绝——这是用户签发较窄密钥时
// 故意接受的降级。对于应接受任一途径（admin session 或 type=admin apikey）
// 的路由，请使用 RequirePlatformAdmin。
func RequireSuperAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ident, ok := FromContext(req.Context())
		if !ok || ident.Role != users.RoleSuperAdmin {
			writeForbidden(w, "super_admin required")
			return
		}
		// Apikey 调用方还必须持有 type=admin 密钥——super_admin
		// 的 type=user 密钥是有意缩窄的。
		if ident.AuthMethod == "apikey" && ident.APIKeyType != users.APIKeyTypeAdmin {
			writeForbidden(w, "admin apikey required")
			return
		}
		next(w, req)
	}
}

// RequirePlatformAdmin 用于门控应接受任何平台管理员（session super_admin
// 或 type=admin apikey）的处理程序。在允许的权限方面与 RequireSuperAdmin
// 相同；只是不要求 session 路径。
func RequirePlatformAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ident, ok := FromContext(req.Context())
		if !ok || !ident.CanAdminPlatform() {
			writeForbidden(w, "platform admin required")
			return
		}
		next(w, req)
	}
}

// RequireWritable 拒绝 Identity.ReadOnly() 为 true（即调用方正在模拟
// 另一个用户）的请求。包装变更加载处理程序。
func RequireWritable(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ident, ok := FromContext(req.Context())
		if !ok {
			writeUnauthorized(w)
			return
		}
		if ident.ReadOnly() {
			writeForbidden(w, "read-only: cannot mutate while acting as another user")
			return
		}
		next(w, req)
	}
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
}

func writeForbidden(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(`{"ok":false,"error":"` + msg + `"}`))
}

func newSID() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
