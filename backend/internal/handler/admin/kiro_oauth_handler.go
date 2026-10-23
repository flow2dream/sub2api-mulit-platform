package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type KiroOAuthHandler struct{ service *service.KiroOAuthService }

func NewKiroOAuthHandler(s *service.KiroOAuthService) *KiroOAuthHandler {
	return &KiroOAuthHandler{service: s}
}

func kiroOAuthOwner(c *gin.Context) (int64, bool) {
	c.Header("Cache-Control", "no-store")
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "administrator login required")
		return 0, false
	}
	return subject.UserID, true
}
func (h *KiroOAuthHandler) Start(c *gin.Context) {
	owner, ok := kiroOAuthOwner(c)
	if !ok {
		return
	}
	var req struct {
		AuthRegion string `json:"auth_region"`
		StartURL   string `json:"start_url"`
		ProxyID    *int64 `json:"proxy_id"`
	}
	if c.ShouldBindJSON(&req) != nil {
		response.BadRequest(c, "invalid authorization request")
		return
	}
	result, err := h.service.Start(c.Request.Context(), owner, req.AuthRegion, req.StartURL, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
func (h *KiroOAuthHandler) Poll(c *gin.Context) {
	owner, ok := kiroOAuthOwner(c)
	if !ok {
		return
	}
	var req struct {
		SessionID string `json:"session_id" binding:"required"`
	}
	if c.ShouldBindJSON(&req) != nil {
		response.BadRequest(c, "session_id is required")
		return
	}
	result, err := h.service.Poll(c.Request.Context(), owner, req.SessionID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
