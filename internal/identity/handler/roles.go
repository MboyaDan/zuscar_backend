package handler

// Role and permission administration endpoints (Stage 7b, docs/api/routes.csv).

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"zuscar/backend/internal/platform/auth"
	zhttp "zuscar/backend/internal/platform/http"
)

func (h *Handler) registerRoles(v1 *gin.RouterGroup) {
	guard := []gin.HandlerFunc{h.Authenticate(false), h.CSRF()}
	r := v1.Group("/roles", guard...)
	r.GET("", h.requirePermission("roles.manage"), h.listRoles)
	r.POST("", h.requirePermission("roles.manage"), h.createRole)
	r.PATCH("/:id", h.requirePermission("roles.manage"), h.updateRole)
	r.DELETE("/:id", h.requirePermission("roles.manage"), h.deleteRole)
	r.PUT("/:id/permissions", h.requirePermission("permissions.assign"), h.setRolePermissions)
	v1.GET("/permissions", append(guard, h.requirePermission("roles.manage"), h.listPermissions)...)
}

func roleID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That role does not exist.")
		return 0, false
	}
	return id, true
}

func (h *Handler) listRoles(c *gin.Context) {
	items, err := h.svc.ListRoles(c.Request.Context(), auth.From(c))
	if err != nil {
		h.adminError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) listPermissions(c *gin.Context) {
	items, err := h.svc.ListPermissions(c.Request.Context(), auth.From(c))
	if err != nil {
		h.adminError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) createRole(c *gin.Context) {
	var in struct {
		Code        string `json:"code"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decode(c, &in) || !required(c, map[string]string{"code": in.Code, "name": in.Name}, 1024) {
		return
	}
	role, err := h.svc.CreateRole(c.Request.Context(), auth.From(c), in.Code, in.Name, in.Description, clientInfo(c))
	if err != nil {
		h.adminError(c, err)
		return
	}
	c.Header("Location", "/api/v1/roles/"+strconv.FormatInt(role.ID, 10))
	c.JSON(http.StatusCreated, gin.H{"role": role})
}

func (h *Handler) updateRole(c *gin.Context) {
	id, ok := roleID(c)
	if !ok {
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decode(c, &in) || !required(c, map[string]string{"name": in.Name}, 1024) {
		return
	}
	role, err := h.svc.UpdateRole(c.Request.Context(), auth.From(c), id, in.Name, in.Description, clientInfo(c))
	if err != nil {
		h.adminError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"role": role})
}

func (h *Handler) deleteRole(c *gin.Context) {
	id, ok := roleID(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteRole(c.Request.Context(), auth.From(c), id, clientInfo(c)); err != nil {
		h.adminError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) setRolePermissions(c *gin.Context) {
	id, ok := roleID(c)
	if !ok {
		return
	}
	var in struct {
		Permissions []string `json:"permissions"`
	}
	if !decode(c, &in) {
		return
	}
	if in.Permissions == nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "permissions", Message: "is required (use an empty list to remove every permission)"})
		return
	}
	role, affected, err := h.svc.SetRolePermissions(c.Request.Context(), auth.From(c), id, in.Permissions, clientInfo(c))
	if err != nil {
		h.adminError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"role": role, "affected_users": affected})
}
