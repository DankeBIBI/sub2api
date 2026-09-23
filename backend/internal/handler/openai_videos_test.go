package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newOpenAIVideoHandlerTestContext(
	t *testing.T,
	method string,
	target string,
	body []byte,
	group *service.Group,
) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, target, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	groupID := int64(7101)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      7102,
		GroupID: &groupID,
		Group:   group,
		User:    &service.User{ID: 7103, Status: service.StatusActive},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7103, Concurrency: 1})
	return c, rec
}

// 门控只用到依赖检查/媒体总闸，后续调度依赖不必装配。
func newOpenAIVideoGateTestHandler() *OpenAIGatewayHandler {
	return &OpenAIGatewayHandler{
		gatewayService:      &service.OpenAIGatewayService{},
		billingCacheService: &service.BillingCacheService{},
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   &ConcurrencyHelper{concurrencyService: &service.ConcurrencyService{}},
	}
}

func TestOpenAIVideoGeneration_MissingModelRejected(t *testing.T) {
	c, rec := newOpenAIVideoHandlerTestContext(
		t, http.MethodPost, "/v1/videos", []byte(`{"prompt":"waves"}`),
		&service.Group{ID: 7101, Platform: service.PlatformOpenAI, AllowImageGeneration: true},
	)

	newOpenAIVideoGateTestHandler().OpenAIVideoGeneration(c)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Contains(t, rec.Body.String(), "model is required")
}

func TestOpenAIVideoGeneration_DisabledGroupRejected(t *testing.T) {
	c, rec := newOpenAIVideoHandlerTestContext(
		t, http.MethodPost, "/v1/videos", []byte(`{"model":"veo3","prompt":"waves"}`),
		&service.Group{ID: 7101, Platform: service.PlatformOpenAI, AllowImageGeneration: false},
	)

	newOpenAIVideoGateTestHandler().OpenAIVideoGeneration(c)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "permission_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Contains(t, rec.Body.String(), service.ImageGenerationPermissionMessage())
}

func TestOpenAIVideoStatus_MissingRequestIDRejected(t *testing.T) {
	c, rec := newOpenAIVideoHandlerTestContext(
		t, http.MethodGet, "/v1/videos/", nil,
		&service.Group{ID: 7101, Platform: service.PlatformOpenAI, AllowImageGeneration: true},
	)

	newOpenAIVideoGateTestHandler().OpenAIVideoStatus(c)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "request_id is required")
}
