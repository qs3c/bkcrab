package setup

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Middleware 将三种认证级别打包在一起，在构造时注入给各域 handler，
// 让每个 handler 在 RegisterRoutes 中自行组合路由权限，无需依赖 Server。
type Middleware struct {
	// Auth 要求请求方携带有效凭证（session cookie 或 API key）。
	Auth func(http.HandlerFunc) http.HandlerFunc
	// Opt 是引导友好变体：有凭证则填充 Identity，无凭证则放行。
	Opt func(http.HandlerFunc) http.HandlerFunc
	// Admin 要求平台管理员权限（super_admin session 或 type=admin API key）。
	Admin func(http.HandlerFunc) http.HandlerFunc
}

// Handler 是所有域 handler 必须实现的接口，对标 webook 风格。
// RegisterRoutes 将该 handler 的全部端点挂载到 gin 引擎上。
type Handler interface {
	RegisterRoutes(r *gin.Engine)
}
