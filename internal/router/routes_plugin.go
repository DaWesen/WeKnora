package router

import (
	"net/http"

	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

// RegisterPluginRoutes registers the deployment-level external plugin control
// plane. Plugin state is process-scoped, so all routes require SystemAdmin.
func RegisterPluginRoutes(r *gin.RouterGroup, handler *handler.PluginHandler, g *rbacGuards) {
	plugins := r.Group("/system/admin/plugins", g.SystemAdmin())
	readRoutes := g.apiKeyGroup(plugins, apiKeyPlatform(
		types.APIKeyCapabilitySystemRuntimeRead,
		types.APIKeyCapabilitySystemRuntimeManage,
	))
	readRoutes.GET("", handler.List)
	readRoutes.GET("/:id", handler.Get)
	readRoutes.GET("/:id/audit", handler.ListAudit)
	g.apiKeyRoute(
		plugins,
		http.MethodPost,
		"/:id/restart",
		apiKeyPlatform(types.APIKeyCapabilitySystemRuntimeManage),
		handler.Restart,
	)
}
