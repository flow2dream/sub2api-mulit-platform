//go:build unit

package admin

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestKiroDeviceOAuthHandlerRequiresOwnerAndDisablesCaching(t *testing.T) {
	for _, action := range []string{"start", "poll"} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
		h := NewKiroOAuthHandler(nil)
		if action == "start" {
			h.Start(c)
		} else {
			h.Poll(c)
		}
		require.Equal(t, 401, rec.Code)
		require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"session_id":"invalid"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	NewKiroOAuthHandler(service.NewKiroOAuthService(nil, nil, nil)).Poll(c)
	require.Equal(t, 400, rec.Code)
	require.NotContains(t, rec.Body.String(), "access_token")
}
