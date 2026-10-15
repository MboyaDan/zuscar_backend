package handler

// User administration endpoints (Stage 7, docs/api/routes.csv rows for /users). Each route checks its permission in
// the middleware and again inside the service, so a forgotten route guard never opens an operation (AC-10).

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"zuscar/backend/internal/identity/service"
	"zuscar/backend/internal/platform/auth"
	zhttp "zuscar/backend/internal/platform/http"
)

// RegisterAdmin mounts /users on the /api/v1 group. A pending forced password change blocks all of it.
func (h *Handler) RegisterAdmin(v1 *gin.RouterGroup) {
	u := v1.Group("/users", h.Authenticate(false), h.CSRF())
	u.GET("", h.requirePermission("users.update"), h.listUsers)
	u.POST("", h.requirePermission("users.create"), h.createUser)
	u.GET("/:id", h.requirePermission("users.update"), h.getUser)
	u.PATCH("/:id", h.requirePermission("users.update"), h.updateUser)
	u.PUT("/:id/roles", h.requirePermission("users.update"), h.setUserRoles)
	u.POST("/:id/deactivate", h.requirePermission("users.deactivate"), h.deactivateUser)
	u.POST("/:id/reactivate", h.requirePermission("users.update"), h.reactivateUser)
	u.POST("/:id/reset-password", h.requirePermission("users.update"), h.resetPassword)
	u.PUT("/:id/pin", h.requirePermission("users.update"), h.setPIN)
	u.DELETE("/:id", h.requirePermission("users.delete"), h.deleteUser)
	h.registerRoles(v1)
}

// requirePermission answers 403 when the signed-in user lacks the permission (401 is handled by Authenticate).
func (h *Handler) requirePermission(permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !auth.From(c).Has(permission) {
			zhttp.WriteError(c, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
			return
		}
		c.Next()
	}
}

func userID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That user does not exist.")
		return 0, false
	}
	return id, true
}

func (h *Handler) listUsers(c *gin.Context) {
	f := service.ListFilter{Status: c.Query("status"), Search: c.Query("q")}
	if f.Status != "" && f.Status != "active" && f.Status != "inactive" {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "status", Message: "must be active or inactive"})
		return
	}
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "limit", Message: "must be 1 to 200"})
			return
		}
		f.Limit = n
	}
	if cur := c.Query("cursor"); cur != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cur)
		after, perr := strconv.ParseInt(string(raw), 10, 64)
		if err != nil || perr != nil || after < 0 {
			zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "cursor", Message: "is not valid"})
			return
		}
		f.AfterID = after
	}
	items, next, err := h.svc.ListUsers(c.Request.Context(), auth.From(c), f)
	if err != nil {
		h.adminError(c, err)
		return
	}
	cursor := ""
	if next != 0 {
		cursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(next, 10)))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "next_cursor": cursor})
}

func (h *Handler) getUser(c *gin.Context) {
	id, ok := userID(c)
	if !ok {
		return
	}
	u, err := h.svc.GetUser(c.Request.Context(), auth.From(c), id)
	if err != nil {
		h.adminError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": u})
}

func (h *Handler) createUser(c *gin.Context) {
	var in struct {
		Username    string   `json:"username"`
		DisplayName string   `json:"display_name"`
		Password    string   `json:"password"`
		Roles       []string `json:"roles"`
	}
	if !decode(c, &in) || !required(c, map[string]string{"username": in.Username, "display_name": in.DisplayName, "password": in.Password}, 1024) {
		return
	}
	u, err := h.svc.CreateUser(c.Request.Context(), auth.From(c), service.NewUser{Username: in.Username, DisplayName: in.DisplayName, Password: in.Password, Roles: in.Roles}, clientInfo(c))
	if err != nil {
		h.adminError(c, err)
		return
	}
	c.Header("Location", "/api/v1/users/"+strconv.FormatInt(u.ID, 10))
	c.JSON(http.StatusCreated, gin.H{"user": u})
}

func (h *Handler) updateUser(c *gin.Context) {
	id, ok := userID(c)
	if !ok {
		return
	}
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if !decode(c, &in) || !required(c, map[string]string{"display_name": in.DisplayName}, 1024) {
		return
	}
	u, err := h.svc.UpdateUser(c.Request.Context(), auth.From(c), id, in.DisplayName, clientInfo(c))
	if err != nil {
		h.adminError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": u})
}

func (h *Handler) setUserRoles(c *gin.Context) {
	id, ok := userID(c)
	if !ok {
		return
	}
	var in struct {
		Roles []string `json:"roles"`
	}
	if !decode(c, &in) {
		return
	}
	if in.Roles == nil {
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "roles", Message: "is required (use an empty list to remove every role)"})
		return
	}
	u, err := h.svc.SetUserRoles(c.Request.Context(), auth.From(c), id, in.Roles, clientInfo(c))
	if err != nil {
		h.adminError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": u})
}

func (h *Handler) deactivateUser(c *gin.Context) {
	id, ok := userID(c)
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if c.Request.ContentLength != 0 && !decode(c, &in) {
		return
	}
	u, err := h.svc.Deactivate(c.Request.Context(), auth.From(c), id, in.Reason, clientInfo(c))
	if err != nil {
		h.adminError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": u})
}

func (h *Handler) reactivateUser(c *gin.Context) {
	id, ok := userID(c)
	if !ok {
		return
	}
	u, err := h.svc.Reactivate(c.Request.Context(), auth.From(c), id, clientInfo(c))
	if err != nil {
		h.adminError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": u})
}

func (h *Handler) resetPassword(c *gin.Context) {
	id, ok := userID(c)
	if !ok {
		return
	}
	var in struct {
		NewPassword string `json:"new_password"`
	}
	if !decode(c, &in) || !required(c, map[string]string{"new_password": in.NewPassword}, 1024) {
		return
	}
	if err := h.svc.ResetPassword(c.Request.Context(), auth.From(c), id, in.NewPassword, clientInfo(c)); err != nil {
		h.adminError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) setPIN(c *gin.Context) {
	id, ok := userID(c)
	if !ok {
		return
	}
	var in struct {
		PIN string `json:"pin"`
	}
	if !decode(c, &in) || !required(c, map[string]string{"pin": in.PIN}, 64) {
		return
	}
	if err := h.svc.AdminSetPIN(c.Request.Context(), auth.From(c), id, in.PIN, clientInfo(c)); err != nil {
		h.adminError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) deleteUser(c *gin.Context) {
	id, ok := userID(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteUser(c.Request.Context(), auth.From(c), id, clientInfo(c)); err != nil {
		h.adminError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// adminError maps service errors to the standard error body.
func (h *Handler) adminError(c *gin.Context, err error) {
	var verr *service.ValidationError
	var role *service.UnknownRoleError
	switch {
	case errors.Is(err, service.ErrForbidden):
		zhttp.WriteError(c, http.StatusForbidden, "forbidden", "You do not have permission to do this.")
	case errors.Is(err, service.ErrReauthRequired):
		zhttp.WriteError(c, http.StatusForbidden, "reauth_required", "Please confirm your password to continue.")
	case errors.Is(err, service.ErrProtectedTarget):
		zhttp.WriteError(c, http.StatusForbidden, "protected_account", err.Error()+".")
	case errors.Is(err, service.ErrEscalation):
		zhttp.WriteError(c, http.StatusForbidden, "escalation_refused", err.Error()+".")
	case errors.Is(err, service.ErrProtectedRole), errors.Is(err, service.ErrProtectedPermission):
		zhttp.WriteError(c, http.StatusForbidden, "protected_role", err.Error()+".")
	case errors.Is(err, service.ErrRoleNotFound):
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That role does not exist.")
	case errors.Is(err, service.ErrRoleCodeTaken):
		zhttp.WriteError(c, http.StatusConflict, "role_code_taken", "That role code is already in use.", zhttp.FieldError{Field: "code", Message: "is already in use"})
	case errors.Is(err, service.ErrRoleInUse):
		zhttp.WriteError(c, http.StatusConflict, "role_in_use", err.Error()+".")
	case errors.Is(err, service.ErrUserNotFound):
		zhttp.WriteError(c, http.StatusNotFound, "not_found", "That user does not exist.")
	case errors.Is(err, service.ErrUsernameTaken):
		zhttp.WriteError(c, http.StatusConflict, "username_taken", "That username is already in use.", zhttp.FieldError{Field: "username", Message: "is already in use"})
	case errors.Is(err, service.ErrSelfAction):
		zhttp.WriteError(c, http.StatusConflict, "self_action_refused", err.Error()+".")
	case errors.Is(err, service.ErrLastSuperAdmin):
		zhttp.WriteError(c, http.StatusConflict, "last_super_admin", err.Error()+".")
	case errors.Is(err, service.ErrHasHistory):
		zhttp.WriteError(c, http.StatusConflict, "has_history", err.Error()+".")
	case errors.Is(err, service.ErrPINNotAllowed):
		zhttp.WriteError(c, http.StatusConflict, "pin_not_allowed", err.Error()+".")
	case errors.Is(err, service.ErrInvalidPIN):
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "pin", Message: err.Error()})
	case errors.As(err, &verr):
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: verr.Field, Message: verr.Msg})
	case errors.As(err, &role):
		zhttp.WriteError(c, http.StatusBadRequest, "validation_failed", "Some fields are invalid.", zhttp.FieldError{Field: "roles", Message: "unknown role: " + role.Code})
	default:
		h.internal(c, err)
	}
}
