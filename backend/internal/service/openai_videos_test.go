package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newOpenAIVideoTestService(upstream HTTPUpstream) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false},
			},
		},
	}
}

func newOpenAIVideoAPIKeyAccount() *Account {
	return &Account{
		ID:       61,
		Name:     "openai-apikey-videos",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://up.example/v1",
		},
	}
}

func newOpenAIVideoTestContext(t *testing.T, method, target string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	return c, rec
}

// image2api 风格创建响应：id/status/model/seconds（字符串数字）/size（宽x高）。
func openAIVideoCreateResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Request-Id": []string{"req_video_create"},
		},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"video-abc","object":"video","status":"queued","progress":0,"model":"veo3","seconds":"8","size":"1280x720"}`,
		)),
	}
}

// 账号自带自定义 base_url（https://up.example/v1）时，上游必须收到 {base}/videos。
func TestForwardOpenAIVideoJSON_APIKeyUsesConfiguredV1BaseURL(t *testing.T) {
	body := []byte(`{"model":"veo3","prompt":"waves","resolution":"720p","duration":8}`)
	c, rec := newOpenAIVideoTestContext(t, http.MethodPost, "/v1/videos", body)
	upstream := &httpUpstreamRecorder{resp: openAIVideoCreateResponse()}
	svc := newOpenAIVideoTestService(upstream)

	result, err := svc.ForwardOpenAIVideoJSON(
		context.Background(), c, newOpenAIVideoAPIKeyAccount(), OpenAIVideoEndpointCreate, "", body, "application/json",
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "video-abc", result.ResponseID)
	require.Zero(t, result.VideoCount, "创建阶段不得带上可计费单位（成功才计费）")
	require.Equal(t, VideoBillingResolution720P, result.VideoResolution)
	require.Equal(t, 8, result.VideoDurationSeconds)

	require.NotNil(t, upstream.lastReq)
	require.Equal(t, http.MethodPost, upstream.lastReq.Method)
	require.Equal(t, "https://up.example/v1/videos", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer sk-test", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "application/json", upstream.lastReq.Header.Get("Content-Type"))
	require.Equal(t, "veo3", gjson.GetBytes(upstream.lastBody, "model").String())

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "video-abc", gjson.Get(rec.Body.String(), "id").String())
}

// 上游 status == "completed" 才带回可计费单位，size 按高度映射到计费档位。
func TestForwardOpenAIVideoJSON_StatusCompletedMarksBillable(t *testing.T) {
	c, rec := newOpenAIVideoTestContext(t, http.MethodGet, "/v1/videos/video-abc", nil)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"video-abc","object":"video","status":"completed","progress":100,"model":"veo3","seconds":"30","size":"1920x1080"}`,
		)),
	}}
	svc := newOpenAIVideoTestService(upstream)

	result, err := svc.ForwardOpenAIVideoJSON(
		context.Background(), c, newOpenAIVideoAPIKeyAccount(), OpenAIVideoEndpointStatus, "video-abc", nil, "",
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, result.VideoCount)
	require.Equal(t, VideoBillingResolution1080P, result.VideoResolution)
	require.Positive(t, result.VideoDurationSeconds)
	require.Equal(t, "https://up.example/v1/videos/video-abc", upstream.lastReq.URL.String())
	require.Equal(t, http.MethodGet, upstream.lastReq.Method)
	require.Equal(t, http.StatusOK, rec.Code)
}

// content 走 .../videos/{id}/content，Range 透传，206 与 Content-Range 原样回写。
func TestForwardOpenAIVideoContent_StreamsRanges(t *testing.T) {
	c, rec := newOpenAIVideoTestContext(t, http.MethodGet, "/v1/videos/video-abc/content", nil)
	c.Request.Header.Set("Range", "bytes=0-12")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusPartialContent,
		Header: http.Header{
			"Content-Type":        []string{"video/mp4"},
			"Content-Length":      []string{"13"},
			"Content-Range":       []string{"bytes 0-12/100"},
			"Accept-Ranges":       []string{"bytes"},
			"Content-Disposition": []string{`attachment; filename="video-abc.mp4"`},
		},
		Body: io.NopCloser(strings.NewReader("video-payload")),
	}}
	svc := newOpenAIVideoTestService(upstream)

	statusCode, err := svc.ForwardOpenAIVideoContent(
		context.Background(), c, newOpenAIVideoAPIKeyAccount(), "video-abc",
	)

	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, statusCode)
	require.Equal(t, http.StatusPartialContent, rec.Code)
	require.Equal(t, "video-payload", rec.Body.String())

	require.NotNil(t, upstream.lastReq)
	require.Equal(t, http.MethodGet, upstream.lastReq.Method)
	require.Equal(t, "https://up.example/v1/videos/video-abc/content", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer sk-test", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "bytes=0-12", upstream.lastReq.Header.Get("Range"))

	require.Equal(t, "video/mp4", rec.Header().Get("Content-Type"))
	require.Equal(t, "13", rec.Header().Get("Content-Length"))
	require.Equal(t, "bytes 0-12/100", rec.Header().Get("Content-Range"))
	require.Equal(t, "bytes", rec.Header().Get("Accept-Ranges"))
	require.Equal(t, `attachment; filename="video-abc.mp4"`, rec.Header().Get("Content-Disposition"))
	require.True(t, IsResponseCommitted(c))
}

// 计费档位映射：宽x高按高度向上取档，已归一化取值保持不变。
func TestOpenAIVideoBillingResolutionFromSize(t *testing.T) {
	for raw, want := range map[string]string{
		"1280x720":  VideoBillingResolution720P,
		"854x480":   VideoBillingResolution480P,
		"1920x1080": VideoBillingResolution1080P,
		"720p":      VideoBillingResolution720P,
		"":          "",
	} {
		require.Equal(t, want, OpenAIVideoBillingResolutionFromSize(raw), "size=%s", raw)
	}
}
