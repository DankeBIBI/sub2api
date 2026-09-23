package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// OpenAIVideoEndpoint 是 OpenAI 风格视频端点的上游路径标识。
//
// 上游是「openai 平台 + AccountTypeAPIKey + 自定义 base_url」的账号
// （image2api）：
//
//	POST {base_url}/videos              创建任务
//	GET  {base_url}/videos/{id}         查询状态
//	GET  {base_url}/videos/{id}/content 下载 mp4
//
// 常量以 /v1 开头，交给 buildOpenAIEndpointURL 去重：base_url 已带 /v1 时
// 只补相对段，否则补完整 /v1/videos。
type OpenAIVideoEndpoint string

const (
	// OpenAIVideoEndpointCreate 创建视频任务（POST /v1/videos）。
	OpenAIVideoEndpointCreate OpenAIVideoEndpoint = "/v1/videos"
	// OpenAIVideoEndpointStatus 查询视频任务状态（GET /v1/videos/{request_id}）。
	OpenAIVideoEndpointStatus OpenAIVideoEndpoint = "/v1/videos/{request_id}"
	// OpenAIVideoEndpointContent 下载视频文件（GET /v1/videos/{request_id}/content）。
	OpenAIVideoEndpointContent OpenAIVideoEndpoint = "/v1/videos/{request_id}/content"

	openAIVideoRequestIDPlaceholder = "{request_id}"
)

// RequiresRequestBody 报告端点是否需要请求体（只有创建请求带 body）。
func (e OpenAIVideoEndpoint) RequiresRequestBody() bool {
	return e == OpenAIVideoEndpointCreate
}

// IsLookup 报告端点是否为无请求体的查询（状态查询 / 内容下载）。
func (e OpenAIVideoEndpoint) IsLookup() bool {
	return e == OpenAIVideoEndpointStatus || e == OpenAIVideoEndpointContent
}

// IsContentDownload 报告端点是否为 mp4 流式下载。
func (e OpenAIVideoEndpoint) IsContentDownload() bool {
	return e == OpenAIVideoEndpointContent
}

func (e OpenAIVideoEndpoint) httpMethod() string {
	if e.RequiresRequestBody() {
		return http.MethodPost
	}
	return http.MethodGet
}

// Path 把 request_id 填进路径占位符，得到上游相对路径。
func (e OpenAIVideoEndpoint) Path(requestID string) string {
	return strings.ReplaceAll(string(e), openAIVideoRequestIDPlaceholder, strings.TrimSpace(requestID))
}

// IsOpenAIVideoStatusCompleted 判定 image2api 风格视频完成状态：status == "completed"。
func IsOpenAIVideoStatusCompleted(statusBody []byte) bool {
	if len(statusBody) == 0 || !gjson.ValidBytes(statusBody) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(gjson.GetBytes(statusBody, "status").String()), "completed")
}

// ExtractOpenAIVideoRequestID 从创建 / 状态响应里取上游任务 id。
func ExtractOpenAIVideoRequestID(body []byte) string {
	return extractGrokMediaVideoRequestID(body)
}

// OpenAIVideoBillingResolutionFromSize 把上游尺寸取值归一化到计费档位。
//
// image2api 的 /videos 响应以 "宽x高" 表示尺寸（如 "1280x720"），xAI 的档位
// 命名（480p/720p/1080p）无法直接套用，因此按高度向上取档：
// <=480 → 480p，<=720 → 720p，其余 → 1080p。已经是档位取值（"720p"）或
// 无法解析时退回既有归一化逻辑（未知档位按最低档兜底）。
func OpenAIVideoBillingResolutionFromSize(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if normalized, ok := LookupVideoBillingResolution(raw); ok {
		return normalized
	}
	parts := strings.SplitN(strings.ToLower(raw), "x", 2)
	if len(parts) != 2 {
		return NormalizeVideoBillingResolutionOrDefault(raw)
	}
	height, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || height <= 0 {
		return NormalizeVideoBillingResolutionOrDefault(raw)
	}
	switch {
	case height <= 480:
		return VideoBillingResolution480P
	case height <= 720:
		return VideoBillingResolution720P
	default:
		return VideoBillingResolution1080P
	}
}

// buildOpenAIVideoURL 拼接上游视频端点 URL。
// 视频端点只支持带自定义 base_url 的 openai APIKey 账号（image2api），
// 没有 base_url 时无法确定上游，直接报错而不是猜官方域名。
func (s *OpenAIGatewayService) buildOpenAIVideoURL(account *Account, endpoint OpenAIVideoEndpoint, requestID string) (string, error) {
	if account == nil {
		return "", fmt.Errorf("openai video account is required")
	}
	base := strings.TrimSpace(account.GetOpenAIBaseURL())
	if base == "" {
		return "", fmt.Errorf("openai video upstream base_url is required")
	}
	validated, err := s.validateUpstreamBaseURL(base)
	if err != nil {
		return "", err
	}
	return buildOpenAIEndpointURL(validated, endpoint.Path(requestID)), nil
}

// buildOpenAIVideoUpstreamRequest 组装发往上游的视频请求（鉴权头 + 白名单透传头 + 账号头覆写）。
func (s *OpenAIGatewayService) buildOpenAIVideoUpstreamRequest(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	endpoint OpenAIVideoEndpoint,
	token string,
	targetURL string,
	body []byte,
	contentType string,
) (*http.Request, error) {
	var bodyReader io.Reader
	if endpoint.RequiresRequestBody() {
		bodyReader = bytes.NewReader(body)
	}
	// 查询路径（status/content，含 mp4 下载）禁用重定向：上游 3xx 一律视为异常，
	// 避免把 Bearer 凭证/Range 协商带到账号 base_url 之外的主机。创建路径对齐
	// 图片转发链（跟随上游自身的重定向）。
	requestCtx := ctx
	if !endpoint.RequiresRequestBody() {
		requestCtx = WithHTTPUpstreamRedirectsDisabled(ctx)
	}
	req, err := http.NewRequestWithContext(requestCtx, endpoint.httpMethod(), targetURL, bodyReader)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	authHeaders, err := s.buildOpenAIAuthenticationHeaders(ctx, account, token)
	if err != nil {
		return nil, fmt.Errorf("build openai authentication headers: %w", err)
	}
	for key, values := range authHeaders {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			if !openaiPassthroughAllowedHeaders[strings.ToLower(key)] {
				continue
			}
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	}
	if customUA := account.GetOpenAIUserAgent(); customUA != "" {
		req.Header.Set("User-Agent", customUA)
	}
	if endpoint.IsContentDownload() {
		req.Header.Set("Accept", "*/*")
		// Range 不在透传白名单里（避免污染普通请求），视频内容下载需要按需放行，
		// 否则客户端断点续传会拿到完整文件。
		if c != nil {
			if rangeHeader := strings.TrimSpace(c.GetHeader("Range")); rangeHeader != "" {
				req.Header.Set("Range", rangeHeader)
			}
		}
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if endpoint.RequiresRequestBody() {
		if strings.TrimSpace(contentType) == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	// 账号级请求头覆写最后应用，配置值优先于内置默认头。
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// ForwardOpenAIVideoJSON 转发 OpenAI 风格视频的创建 / 状态查询请求。
//
// 创建成功后不在此计费：返回的 result 携带上游任务 id 与创建时定价参数
// （resolution / duration），由 handler 绑定账号并写入 pending 快照，
// 等 status/content 观测到完成时再做一次性扣费。
func (s *OpenAIGatewayService) ForwardOpenAIVideoJSON(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	endpoint OpenAIVideoEndpoint,
	requestID string,
	body []byte,
	contentType string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()
	if account == nil {
		return nil, fmt.Errorf("openai video account is required")
	}
	if endpoint.IsContentDownload() {
		return nil, fmt.Errorf("openai video content endpoint must use ForwardOpenAIVideoContent")
	}
	if !endpoint.RequiresRequestBody() && strings.TrimSpace(requestID) == "" {
		return nil, fmt.Errorf("openai video request id is required")
	}
	targetURL, err := s.buildOpenAIVideoURL(account, endpoint, requestID)
	if err != nil {
		return nil, err
	}

	// 长耗时：客户端中途断开不应取消上游请求（上游侧已经产生实际成本）。
	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()

	token, _, err := s.GetAccessToken(upstreamCtx, account)
	if err != nil {
		return nil, err
	}
	upstreamReq, err := s.buildOpenAIVideoUpstreamRequest(upstreamCtx, c, account, endpoint, token, targetURL, body, contentType)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	upstreamStart := time.Now()
	resp, err := s.doOpenAIUpstream(upstreamReq, proxyURL, account)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(upstreamCtx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	upstreamModel := openAIVideoUpstreamModel(account, endpoint, body)
	SetOpsUpstreamModel(c, upstreamModel)
	if resp.StatusCode >= 400 {
		return s.handleOpenAIVideoUpstreamErrorResponse(upstreamCtx, resp, c, account, safeUpstreamURL(upstreamReq.URL.String()), upstreamModel)
	}

	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	s.writeOpenAIVideoJSONResponse(c, resp, respBody)
	return openAIVideoResultFromJSONResponse(endpoint, requestID, body, resp, respBody, startTime), nil
}

// ForwardOpenAIVideoContent 流式转发视频内容（mp4）。
// 返回上游 HTTP 状态码；2xx 视为「视频已完成」的成功观测，供 handler 做一次性计费。
func (s *OpenAIGatewayService) ForwardOpenAIVideoContent(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	requestID string,
) (int, error) {
	if account == nil {
		return 0, fmt.Errorf("openai video account is required")
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return 0, fmt.Errorf("openai video request id is required")
	}
	targetURL, err := s.buildOpenAIVideoURL(account, OpenAIVideoEndpointContent, requestID)
	if err != nil {
		return 0, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()

	token, _, err := s.GetAccessToken(upstreamCtx, account)
	if err != nil {
		return 0, err
	}
	req, err := s.buildOpenAIVideoUpstreamRequest(
		upstreamCtx, c, account, OpenAIVideoEndpointContent, token, targetURL, nil, "",
	)
	if err != nil {
		return 0, err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	upstreamStart := time.Now()
	resp, err := s.doOpenAIUpstream(req, proxyURL, account)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return 0, s.handleOpenAIUpstreamTransportError(upstreamCtx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return 0, fmt.Errorf("openai video content redirect is not allowed")
	}
	// 416 是合法的 Range 协商结果，按内容响应回写（含 Content-Range）让客户端自行处理。
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		_, forwardErr := s.handleOpenAIVideoUpstreamErrorResponse(
			upstreamCtx, resp, c, account, safeUpstreamURL(req.URL.String()), "",
		)
		return 0, forwardErr
	}
	// 与 Grok 媒体内容下载同一套回写口径：透传 Content-Type/Content-Length/
	// Content-Range/Accept-Ranges/Content-Disposition 后 io.Copy 流式拷贝。
	if err := writeGrokMediaContentResponse(c, resp); err != nil {
		return 0, err
	}
	return resp.StatusCode, nil
}

// writeOpenAIVideoJSONResponse 回写上游 JSON 响应，口径对齐
// handleOpenAIImagesNonStreamingResponse（过滤响应头 + 内容类型协商）。
func (s *OpenAIGatewayService) writeOpenAIVideoJSONResponse(c *gin.Context, resp *http.Response, body []byte) {
	if c == nil || resp == nil {
		return
	}
	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	contentType := "application/json"
	if s.cfg != nil && !s.cfg.Security.ResponseHeaders.Enabled {
		if upstreamType := resp.Header.Get("Content-Type"); upstreamType != "" {
			contentType = upstreamType
		}
	}
	c.Data(resp.StatusCode, contentType, body)
}

// handleOpenAIVideoUpstreamErrorResponse 处理上游 >=400 的错误响应，
// 分支判定与 openai_images 的转发链保持一致：
// 可切换 → 类型化 failover 错误（交给 handler 换号）；
// 不可切换 → handleOpenAIImagesErrorResponse 把真实上游错误透出给客户端。
func (s *OpenAIGatewayService) handleOpenAIVideoUpstreamErrorResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	upstreamURL string,
	upstreamModel string,
) (*OpenAIForwardResult, error) {
	respBody := s.readUpstreamErrorBody(resp)
	_ = resp.Body.Close()
	respBody = s.redactAgentIdentitySensitiveBody(ctx, account, respBody)
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)

	if !s.shouldFailoverOpenAIUpstreamResponse(resp.StatusCode, upstreamMsg, respBody) {
		return s.handleOpenAIImagesErrorResponse(ctx, resp, c, account, upstreamModel)
	}

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		UpstreamURL:        upstreamURL,
		Kind:               "failover",
		Message:            upstreamMsg,
	})
	shouldDisable := s.handleFailoverSideEffects(ctx, resp, account, respBody, upstreamModel)
	retryableOnSameAccount := !shouldDisable && account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode)
	if account.IsOpenAIOAuthLike() && resp.StatusCode == http.StatusTooManyRequests {
		return nil, s.newOpenAIAccountFailoverError(account, resp.StatusCode, resp.Header, respBody, upstreamMsg, shouldDisable, retryableOnSameAccount)
	}
	if isOpenAIHTTPUpstreamAccessStateError(resp.StatusCode, upstreamMsg, respBody) {
		return nil, newOpenAIUpstreamFailoverError(resp.StatusCode, resp.Header, respBody, upstreamMsg, retryableOnSameAccount)
	}
	return nil, &UpstreamFailoverError{
		StatusCode:             resp.StatusCode,
		ResponseBody:           respBody,
		ResponseHeaders:        resp.Header.Clone(),
		RetryableOnSameAccount: retryableOnSameAccount,
	}
}

// openAIVideoUpstreamModel 取出用于调度上报的模型：优先上游回传、其次创建请求体。
func openAIVideoUpstreamModel(account *Account, endpoint OpenAIVideoEndpoint, body []byte) string {
	if !endpoint.RequiresRequestBody() {
		return ""
	}
	requestModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if requestModel == "" {
		return ""
	}
	if account == nil {
		return requestModel
	}
	return firstNonEmpty(account.GetMappedModel(requestModel), requestModel)
}

// openAIVideoResultFromJSONResponse 把上游 JSON 响应转成转发结果。
//
// 创建：只带任务 id 与创建时定价参数（不置 VideoCount，异步任务尚未完成）。
// 状态：仅当 status == "completed" 时置 VideoCount = 1，供上层一次性领取计费；
// resolution 取上游 size（"1280x720"），duration 取上游 seconds（字符串数字）。
func openAIVideoResultFromJSONResponse(
	endpoint OpenAIVideoEndpoint,
	requestID string,
	requestBody []byte,
	resp *http.Response,
	respBody []byte,
	startTime time.Time,
) *OpenAIForwardResult {
	result := &OpenAIForwardResult{
		RequestID:       resp.Header.Get("x-request-id"),
		ResponseHeaders: resp.Header.Clone(),
		Duration:        time.Since(startTime),
	}
	usage, _ := extractOpenAIUsageFromJSONBytes(respBody)
	result.Usage = usage
	requestModel := ""
	if endpoint.RequiresRequestBody() {
		requestModel = strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	}
	upstreamModel := strings.TrimSpace(gjson.GetBytes(respBody, "model").String())
	result.Model = firstNonEmpty(upstreamModel, requestModel)
	result.BillingModel = result.Model
	result.UpstreamModel = upstreamModel

	if endpoint.RequiresRequestBody() {
		result.ResponseID = firstNonEmpty(ExtractOpenAIVideoRequestID(respBody), requestID)
		// 计费模型固定用客户端请求的模型：上游可能回传自己的内部命名，
		// 拿它去匹配渠道价格会找不到定价。
		result.BillingModel = firstNonEmpty(requestModel, upstreamModel)
		result.VideoResolution = OpenAIVideoBillingResolutionFromSize(gjson.GetBytes(requestBody, "resolution").String())
		result.VideoDurationSeconds = NormalizeVideoBillingDurationSecondsOrDefault(int(gjson.GetBytes(requestBody, "duration").Int()))
		return result
	}

	result.ResponseID = firstNonEmpty(ExtractOpenAIVideoRequestID(respBody), requestID)
	if !IsOpenAIVideoStatusCompleted(respBody) {
		return result
	}
	result.VideoCount = 1
	if seconds := gjson.GetBytes(respBody, "seconds"); seconds.Exists() {
		result.VideoDurationSeconds = NormalizeVideoBillingDurationSecondsOrDefault(int(seconds.Int()))
	}
	result.VideoResolution = OpenAIVideoBillingResolutionFromSize(gjson.GetBytes(respBody, "size").String())
	return result
}
