// Package ioc 提供基础设施初始化函数，对标 webook 风格的 ioc 分层。
// 职责：组装 gin 引擎所需的全局中间件，以及将 Handler 挂载到引擎的工厂函数。
//
// 依赖注入说明：
//   - 目前采用手动 DI（setup.Server 的 Set*() 方法）。
//   - 如需迁移至 google/wire，只需在此包中声明 wire.Build()；
//     各 NewXxxHandler 构造函数的签名已完全兼容 Wire 的依赖推断。
package ioc

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/qs3c/bkclaw/internal/setup"
)

// InitGinMiddlewares 返回全局 gin 中间件链。
// 认证按路由粒度注入（通过 setup.Middleware），此处仅包含真正全局的中间件。
func InitGinMiddlewares() []gin.HandlerFunc {
	return []gin.HandlerFunc{
		gin.Recovery(),
	}
}

// InitWebServer 创建 gin 引擎，应用全局中间件，并让每个 handler 挂载自己的路由。
// 对标 webook ioc.InitWebServer 的角色——组装入口，不包含业务逻辑。
func InitWebServer(mdls []gin.HandlerFunc, handlers []setup.Handler) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	r.Use(mdls...)

	// 健康探针
	healthz := func(c *gin.Context) { c.String(http.StatusOK, "ok") }
	r.GET("/healthz", healthz)
	r.GET("/livez", healthz)
	r.GET("/readyz", healthz)

	for _, h := range handlers {
		h.RegisterRoutes(r)
	}
	return r
}
