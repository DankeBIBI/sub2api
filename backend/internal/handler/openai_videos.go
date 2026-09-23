package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// OpenAIVideoGeneration 处理 OpenAI 风格视频创建（POST /v1/videos）。
// 上游为「openai 平台 + AccountTypeAPIKey + 自定义 base_url」的账号。
func (h *OpenAIGatewayHandler) OpenAIVideoGeneration(c *gin.Context) {
	h.handleOpenAIVideo(c, service.OpenAIVideoEndpointCreate, "")
}

// OpenAIVideoStatus 处理视频任务状态查询（GET /v1/videos/{request_id}）。
func (h *OpenAIGatewayHandler) OpenAIVideoStatus(c *gin.Context) {
	h.handleOpenAIVideo(c, service.OpenAIVideoEndpointStatus, c.Param("request_id"))
}

// OpenAIVideoContent 处理视频文件下载（GET /v1/videos/{request_id}/content）。
func (h *OpenAIGatewayHandler) OpenAIVideoContent(c *gin.Context) {
	h.handleOpenAIVideo(c, service.OpenAIVideoEndpointContent, c.Param("request_id"))
}

// handleOpenAIVideo 是 OpenAI 风格视频三个端点的公共入口，门控顺序对齐 openai_images：
// 依赖检查 → 读 body → 解析 model → 媒体总闸 → 内容审核 → 并发槽位 → 计费资格 → 选号转发。
//
// 计费语义（成功才计费 / deferred）：
//   - 创建：不扣费，成功后绑定账号 + 写 create 快照（模型/分辨率/时长）。
//   - 状态查询：上游 status == "completed" 时一次性领取并扣费。
//   - 内容下载：HTTP 2xx 同样视为完成，与状态查询共用同一个 claim key，只会扣一次。
func (h *OpenAIGatewayHandler) handleOpenAIVideo(c *gin.Context, endpoint service.OpenAIVideoEndpoint, requestID string) {
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)

	requestStart := time.Now()
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}

	taskRequestID := strings.TrimSpace(requestID)
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.openai_videos",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
		zap.String("endpoint", string(endpoint)),
		zap.String("request_id", taskRequestID),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	var body []byte
	if endpoint.RequiresRequestBody() {
		var err error
		body, err = pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
		if err != nil {
			if maxErr, ok := extractMaxBytesError(err); ok {
				h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
				return
			}
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
			return
		}
		if len(body) == 0 {
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
			return
		}
	}

	contentType := c.GetHeader("Content-Type")
	requestModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if endpoint.RequiresRequestBody() && requestModel == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if endpoint.IsLookup() && taskRequestID == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "request_id is required")
		return
	}
	routingModel := requestModel

	reqLog = reqLog.With(zap.String("model", requestModel))
	setOpsRequestContext(c, requestModel, false)
	setOpsEndpointContext(c, "", int16(service.RequestTypeSync))

	if endpoint.RequiresRequestBody() {
		// 视频复用「允许生成图片」这个媒体总闸（与 Grok 媒体一致，不再新增开关）。
		if !service.GroupAllowsImageGeneration(apiKey.Group) {
			h.errorResponse(c, http.StatusForbidden, "permission_error", service.ImageGenerationPermissionMessage())
			return
		}
		if moderationBody := openAIVideoModerationBody(body); len(moderationBody) > 0 {
			decision := h.checkSecurityAudit(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIImages, requestModel, moderationBody)
			if decision != nil && !decision.AllowNextStage {
				h.openAISecurityAuditError(c, decision)
				return
			}
		}
		imageReleaseFunc, acquired := h.acquireImageGenerationSlot(c, streamStarted)
		if !acquired {
			return
		}
		if imageReleaseFunc != nil {
			defer imageReleaseFunc()
		}
	}

	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai_videos.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.errorResponse(c, status, code, message)
		return
	}

	sessionHash := h.gatewayService.GenerateExplicitSessionHash(c, body)
	boundLookupAccountID := int64(0)
	if endpoint.IsLookup() {
		// status/content 不带 model，必须回到创建时绑定的账号去查，否则会查到别的上游。
		sessionHash = service.GrokMediaVideoRequestSessionHash(taskRequestID, subject.UserID, apiKey.ID)
		resolvedAccountID, resolveErr := h.gatewayService.ResolveGrokMediaVideoRequestAccount(
			c.Request.Context(), apiKey.GroupID, taskRequestID, subject.UserID, apiKey.ID,
		)
		if resolveErr != nil || resolvedAccountID <= 0 {
			reqLog.Info("openai_videos.video_lookup_owner_binding_missing", zap.Error(resolveErr))
			h.errorResponse(c, http.StatusNotFound, "not_found_error", "Video request not found")
			return
		}
		boundLookupAccountID = resolvedAccountID
	}

	// 视频是独立媒体端点，不在 token 利润门范围内：显式豁免，避免在途任务因
	// 绑定账号被门排除而查询返回伪 404。
	requestCtx := service.WithOpenAIProfitControlSuppressed(c.Request.Context())

	maxAccountSwitches := h.maxAccountSwitches
	if maxAccountSwitches <= 0 {
		maxAccountSwitches = 3
	}
	switchCount := 0
	profitVetoCount := 0
	failedAccountIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	var lastFailoverErr *service.UpstreamFailoverError
	var oauth429FailoverState service.OpenAIOAuth429FailoverState
	routingStart := time.Now()
	videoCreateStartedAt := ""
	if endpoint.RequiresRequestBody() {
		videoCreateStartedAt = service.GrokVideoPendingCreatedAtNow()
	}

	for {
		if failoverClientGone(c) {
			return
		}
		// 视频端点只支持 openai APIKey + 自定义 base_url 的账号（image2api）：
		// 能力参数留空，具体限制在选中账号后按类型过滤。
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			requestCtx,
			apiKey.GroupID,
			"",
			sessionHash,
			routingModel,
			failedAccountIDs,
			service.OpenAIUpstreamTransportHTTPSSE,
			"",
			false,
			false,
			false,
			service.PlatformOpenAI,
		)
		if err != nil {
			if failoverClientGone(c) {
				reqLog.Info("openai_videos.account_select_aborted_client_disconnected", zap.Error(err))
				return
			}
			reqLog.Warn("openai_videos.account_select_failed",
				zap.Error(err),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if len(failedAccountIDs) == 0 {
				cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, requestModel, routingModel, service.PlatformOpenAI)
				if !cls.ModelNotFound {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
				}
				h.errorResponse(c, cls.Status, cls.ErrType, cls.Message)
				return
			}
			if lastFailoverErr != nil {
				h.handleFailoverExhausted(c, lastFailoverErr, false)
			} else {
				h.errorResponse(c, http.StatusBadGateway, "api_error", "Upstream request failed")
			}
			return
		}
		if selection == nil || selection.Account == nil {
			cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, requestModel, routingModel, service.PlatformOpenAI)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimited(c)
			}
			h.errorResponse(c, cls.Status, cls.ErrType, cls.Message)
			return
		}
		if boundLookupAccountID > 0 && selection.Account.ID != boundLookupAccountID {
			reqLog.Warn("openai_videos.video_lookup_bound_account_unavailable",
				zap.Int64("bound_account_id", boundLookupAccountID),
				zap.Int64("selected_account_id", selection.Account.ID),
			)
			h.errorResponse(c, http.StatusNotFound, "not_found_error", "Video request not found")
			return
		}

		reqLog.Debug("openai_videos.account_schedule_decision",
			zap.String("layer", scheduleDecision.Layer),
			zap.Bool("sticky_session_hit", scheduleDecision.StickySessionHit),
			zap.Int("candidate_count", scheduleDecision.CandidateCount),
			zap.Int("top_k", scheduleDecision.TopK),
			zap.Int64("latency_ms", scheduleDecision.LatencyMs),
			zap.Float64("load_skew", scheduleDecision.LoadSkew),
		)

		account := selection.Account
		if !openAIVideoAccountEligible(account) {
			// OAuth / 无 base_url 的账号没有可用的视频上游地址：排除后换号，
			// 避免直接把「配置缺失」暴露成上游 502。
			failedAccountIDs[account.ID] = struct{}{}
			reqLog.Warn("openai_videos.account_ineligible",
				zap.Int64("account_id", account.ID),
				zap.String("account_type", string(account.Type)),
			)
			if switchCount >= maxAccountSwitches {
				markOpsRoutingCapacityLimited(c)
				h.errorResponse(c, http.StatusServiceUnavailable, "openai_videos_no_eligible_account", "No eligible OpenAI video accounts")
				return
			}
			switchCount++
			continue
		}
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		setOpsSelectedAccount(c, account.ID, account.Platform)

		accountReleaseFunc, slotResult := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, false, &streamStarted, reqLog)
		if slotResult == openAISlotAcquireProfitVetoed {
			// 视频路径已显式豁免利润门，此分支仅防御性兜底，同样受否决上限约束。
			if !recordOpenAIProfitVeto(failedAccountIDs, account.ID, &profitVetoCount) {
				h.handleOpenAIProfitVetoExhausted(c, streamStarted, reqLog, profitVetoCount)
				return
			}
			continue
		}
		if slotResult != openAISlotAcquireOK {
			return
		}

		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()
		writerSizeBeforeForward := c.Writer.Size()
		result, contentStatusCode, err := func() (*service.OpenAIForwardResult, int, error) {
			defer func() {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
			}()
			if endpoint.IsContentDownload() {
				status, forwardErr := h.gatewayService.ForwardOpenAIVideoContent(requestCtx, c, account, taskRequestID)
				return nil, status, forwardErr
			}
			forwarded, forwardErr := h.gatewayService.ForwardOpenAIVideoJSON(
				requestCtx, c, account, endpoint, taskRequestID, body, contentType,
			)
			return forwarded, 0, forwardErr
		}()

		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		upstreamLatencyMs, _ := getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)

		if err != nil {
			var failoverErr *service.UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				if failoverClientGone(c) {
					reqLog.Info("openai_videos.failover_aborted_client_disconnected",
						zap.Int64("account_id", account.ID),
						zap.Int("upstream_status", failoverErr.StatusCode),
					)
					return
				}
				if failoverErr.ShouldReportAccountScheduleFailure() {
					h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, routingModel, false, nil), false, nil)
				}
				if c.Writer.Size() != writerSizeBeforeForward {
					h.handleFailoverExhausted(c, failoverErr, true)
					return
				}
				if !failoverErr.ShouldRetryNextAccount() {
					h.handleFailoverExhausted(c, failoverErr, false)
					return
				}
				if endpoint.IsLookup() {
					// 查询必须回到创建时绑定的账号，换号只会拿到 404。
					h.handleFailoverExhausted(c, failoverErr, false)
					return
				}
				if failoverErr.RetryableOnSameAccount {
					retryLimit := effectiveSameAccountRetryLimit(failoverErr, account)
					if sameAccountRetryAllowed(failoverErr, sameAccountRetryCount[account.ID], retryLimit) {
						sameAccountRetryCount[account.ID]++
						retryDelay := sameAccountRetryDelayFor(failoverErr, sameAccountRetryCount[account.ID])
						reqLog.Warn("openai_videos.pool_mode_same_account_retry",
							zap.Int64("account_id", account.ID),
							zap.Int("upstream_status", failoverErr.StatusCode),
							zap.Int("retry_limit", retryLimit),
							zap.Int("retry_count", sameAccountRetryCount[account.ID]),
							zap.Duration("retry_delay", retryDelay),
						)
						select {
						case <-requestCtx.Done():
							return
						case <-time.After(retryDelay):
						}
						continue
					}
				}
				h.gatewayService.RecordOpenAIAccountSwitch()
				failedAccountIDs[account.ID] = struct{}{}
				lastFailoverErr = failoverErr
				if switchCount >= maxAccountSwitches {
					h.handleFailoverExhausted(c, failoverErr, false)
					return
				}
				switchCount++
				if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount, &oauth429FailoverState) {
					h.handleFailoverExhausted(c, failoverErr, false)
					return
				}
				reqLog.Warn("openai_videos.upstream_failover_switching",
					zap.Int64("account_id", account.ID),
					zap.Int("upstream_status", failoverErr.StatusCode),
					zap.Int("switch_count", switchCount),
					zap.Int("max_switches", maxAccountSwitches),
				)
				continue
			}
			h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, routingModel, false, nil), false, nil)
			if !service.IsResponseCommitted(c) && c.Writer.Size() == writerSizeBeforeForward {
				h.errorResponse(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
			}
			reqLog.Warn("openai_videos.forward_failed",
				zap.Int64("account_id", account.ID),
				zap.Error(err),
			)
			return
		}

		h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, routingModel, false, result), true, nil)

		if endpoint.RequiresRequestBody() && result != nil {
			h.bindOpenAIVideoCreateResult(requestCtx, c, reqLog, apiKey, subject, account, result, requestModel, videoCreateStartedAt)
		}

		if endpoint.IsLookup() {
			completion := result
			if endpoint.IsContentDownload() {
				if contentStatusCode < http.StatusOK || contentStatusCode >= http.StatusMultipleChoices {
					// 416 等非成功状态回写给了客户端，但不是「视频已完成」的观测，不计费。
					reqLog.Debug("openai_videos.content_not_billable",
						zap.Int("upstream_status", contentStatusCode),
						zap.String("request_id", taskRequestID),
					)
					return
				}
				// 内容下载成功（2xx）等价于已完成：用创建快照计费，claim 与 status 路径共用。
				completion = &service.OpenAIForwardResult{
					ResponseID: taskRequestID,
					VideoCount: 1,
				}
			}
			if billResult := prepareOpenAIVideoCompletionBilling(requestCtx, h, reqLog, apiKey, subject, taskRequestID, completion); billResult != nil {
				recordOpenAIVideoUsage(c, h, reqLog, apiKey, subject, subscription, account, billResult, billResult.Model, body, taskRequestID)
			}
		}
		reqLog.Debug("openai_videos.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}

// openAIVideoAccountEligible 报告账号能否承载 OpenAI 风格视频转发。
// 本期只支持「openai 平台 + APIKey + 自定义 base_url」（上游 image2api）。
func openAIVideoAccountEligible(account *service.Account) bool {
	if account == nil || !account.IsOpenAIApiKey() {
		return false
	}
	return strings.TrimSpace(account.GetOpenAIBaseURL()) != ""
}

// openAIVideoModerationBody 构造视频创建请求的送审内容（当前只有 prompt）。
func openAIVideoModerationBody(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil
	}
	prompt := strings.TrimSpace(gjson.GetBytes(body, "prompt").String())
	if prompt == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]string{"prompt": prompt})
	if err != nil {
		return nil
	}
	return payload
}

// bindOpenAIVideoCreateResult 在创建成功后绑定任务所属账号并落 create 快照。
// 后续 status/content 依赖绑定找到同一上游账号；计费依赖快照补
// resolution/duration（status 可能缺字段）。
func (h *OpenAIGatewayHandler) bindOpenAIVideoCreateResult(
	ctx context.Context,
	c *gin.Context,
	reqLog *zap.Logger,
	apiKey *service.APIKey,
	subject middleware2.AuthSubject,
	account *service.Account,
	result *service.OpenAIForwardResult,
	requestModel string,
	createStartedAt string,
) {
	taskRequestID := strings.TrimSpace(result.ResponseID)
	if taskRequestID == "" {
		return
	}
	if err := h.gatewayService.BindGrokMediaVideoRequestAccount(
		ctx, apiKey.GroupID, taskRequestID, subject.UserID, apiKey.ID, account.ID,
	); err != nil {
		reqLog.Warn("openai_videos.bind_video_request_account_failed",
			zap.Int64("account_id", account.ID),
			zap.String("request_id", taskRequestID),
			zap.Error(err),
		)
	}
	pending := service.GrokVideoPendingBilling{
		Model:                requestModel,
		BillingModel:         firstNonEmptyString(result.BillingModel, requestModel),
		UpstreamModel:        result.UpstreamModel,
		VideoResolution:      result.VideoResolution,
		VideoDurationSeconds: result.VideoDurationSeconds,
		OriginalModel:        clientRequestedModel(c, requestModel),
		// 端到端耗时起点：create 被接受 → 首次观测到完成。
		CreatedAt: createStartedAt,
	}
	if err := h.gatewayService.StoreGrokVideoPendingBilling(ctx, taskRequestID, subject.UserID, apiKey.ID, pending); err != nil {
		reqLog.Warn("openai_videos.store_video_pending_billing_failed_retrying",
			zap.Int64("account_id", account.ID),
			zap.String("request_id", taskRequestID),
			zap.Error(err),
		)
		if err2 := h.gatewayService.StoreGrokVideoPendingBilling(ctx, taskRequestID, subject.UserID, apiKey.ID, pending); err2 != nil {
			// 响应体可能已经写出，完成路径会在缺少快照时按上游 status 的
			// size/seconds 兜底计费。
			reqLog.Error("openai_videos.store_video_pending_billing_failed",
				zap.Int64("account_id", account.ID),
				zap.String("request_id", taskRequestID),
				zap.Error(err2),
			)
		}
	}
}

// prepareOpenAIVideoCompletionBilling 为「完成观测」做一次性领取计费。
// 成功判据：status == "completed"（service 置 VideoCount=1）或 content 下载 2xx
// （handler 传入 VideoCount=1 的合成结果）。claim 失败/已领取则不重复扣费。
func prepareOpenAIVideoCompletionBilling(
	ctx context.Context,
	h *OpenAIGatewayHandler,
	reqLog *zap.Logger,
	apiKey *service.APIKey,
	subject middleware2.AuthSubject,
	taskRequestID string,
	completion *service.OpenAIForwardResult,
) *service.OpenAIForwardResult {
	if h == nil || h.gatewayService == nil || apiKey == nil || completion == nil {
		return nil
	}
	if completion.VideoCount <= 0 {
		return nil
	}
	taskRequestID = strings.TrimSpace(firstNonEmptyString(taskRequestID, completion.ResponseID))
	if taskRequestID == "" {
		return nil
	}
	pending, loadErr := h.gatewayService.LoadGrokVideoPendingBilling(ctx, taskRequestID, subject.UserID, apiKey.ID)
	if loadErr != nil {
		reqLog.Warn("openai_videos.video_pending_billing_load_failed",
			zap.String("request_id", taskRequestID),
			zap.Error(loadErr),
		)
	}
	merged := *completion
	if pending != nil {
		// 计费模型优先取创建时的客户端模型：上游 status 里的 model 可能是上游内部
		// 命名，拿它定价会找不到渠道价格。
		merged.BillingModel = firstNonEmptyString(pending.BillingModel, pending.Model, merged.BillingModel, merged.Model)
		if strings.TrimSpace(merged.Model) == "" {
			merged.Model = firstNonEmptyString(pending.Model, pending.BillingModel, pending.OriginalModel)
		}
		if strings.TrimSpace(merged.UpstreamModel) == "" {
			merged.UpstreamModel = pending.UpstreamModel
		}
		// 完成观测（status.size / 请求体）优先，创建快照补缺。
		if strings.TrimSpace(merged.VideoResolution) == "" {
			merged.VideoResolution = pending.VideoResolution
		}
		if merged.VideoDurationSeconds <= 0 {
			merged.VideoDurationSeconds = pending.VideoDurationSeconds
		}
	}
	if strings.TrimSpace(merged.BillingModel) == "" {
		merged.BillingModel = strings.TrimSpace(merged.Model)
	}
	if strings.TrimSpace(merged.BillingModel) == "" {
		// 没有可定价的模型（快照丢失且上游 status 也无 model）：不领取 claim，
		// 留给下一次轮询（快照可能仍在 TTL 内）。
		reqLog.Error("openai_videos.video_billing_skipped_missing_model",
			zap.String("request_id", taskRequestID),
		)
		return nil
	}
	// 领取放在模型校验之后：拿不到模型时不该烧掉唯一一次扣费机会。
	claimed, err := h.gatewayService.ClaimGrokVideoBilling(ctx, taskRequestID, subject.UserID, apiKey.ID)
	if err != nil {
		reqLog.Warn("openai_videos.video_billing_claim_failed",
			zap.String("request_id", taskRequestID),
			zap.Error(err),
		)
		return nil
	}
	if !claimed {
		reqLog.Debug("openai_videos.video_billing_already_claimed", zap.String("request_id", taskRequestID))
		return nil
	}
	if strings.TrimSpace(merged.Model) == "" {
		merged.Model = merged.BillingModel
	}
	// 稳定任务 id：多次 status/content 轮询共用同一条用量去重键。
	merged.RequestID = service.StableGrokVideoBillingRequestID(firstNonEmptyString(merged.ResponseID, taskRequestID))
	merged.ResponseID = firstNonEmptyString(merged.ResponseID, taskRequestID)
	merged.VideoCount = 1
	// 纯视频：清掉图片口径，避免落到图片计价分支。
	merged.ImageCount = 0
	merged.VideoResolution = service.NormalizeVideoBillingResolutionOrDefault(merged.VideoResolution)
	merged.VideoDurationSeconds = service.NormalizeVideoBillingDurationSecondsOrDefault(merged.VideoDurationSeconds)
	if pending != nil {
		// 异步视频的端到端耗时：create 被接受 → 本次观测到完成。
		if e2e := service.GrokVideoE2EDuration(pending.CreatedAt, time.Now()); e2e > 0 {
			merged.Duration = e2e
		}
	}
	return &merged
}

// recordOpenAIVideoUsage 上报视频完成用量；RecordUsage 失败时释放 claim 以便下次轮询重试。
func recordOpenAIVideoUsage(
	c *gin.Context,
	h *OpenAIGatewayHandler,
	reqLog *zap.Logger,
	apiKey *service.APIKey,
	subject middleware2.AuthSubject,
	subscription *service.UserSubscription,
	account *service.Account,
	result *service.OpenAIForwardResult,
	requestModel string,
	body []byte,
	requestID string,
) {
	userAgent := c.GetHeader("User-Agent")
	clientIP := ip.GetClientIP(c)
	sessionID := service.ExtractClientSessionID(c)
	payloadForHash := body
	if len(payloadForHash) == 0 && strings.TrimSpace(requestID) != "" {
		payloadForHash = []byte(requestID)
	}
	inboundEndpoint := GetInboundEndpoint(c)
	upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
	quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
	channelUsageFields := service.ChannelUsageFields{
		OriginalModel:      clientRequestedModel(c, requestModel),
		ChannelMappedModel: requestModel,
	}
	videoTaskID := strings.TrimSpace(firstNonEmptyString(requestID, result.ResponseID))
	if stable := service.StableGrokVideoBillingRequestID(firstNonEmptyString(result.ResponseID, requestID)); stable != "" {
		result.RequestID = stable
	}
	if len(body) == 0 && videoTaskID != "" {
		payloadForHash = []byte(videoTaskID)
	}
	h.submitOpenAIUsageRecordTask(c.Request.Context(), result, func(ctx context.Context) {
		if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
			Result:             result,
			APIKey:             apiKey,
			User:               apiKey.User,
			Account:            account,
			Subscription:       subscription,
			InboundEndpoint:    inboundEndpoint,
			UpstreamEndpoint:   upstreamEndpoint,
			UserAgent:          userAgent,
			IPAddress:          clientIP,
			RequestPayloadHash: service.HashUsageRequestPayload(payloadForHash),
			APIKeyService:      h.apiKeyService,
			QuotaPlatform:      quotaPlatform,
			SessionID:          sessionID,
			ChannelUsageFields: channelUsageFields,
		}); err != nil {
			if videoTaskID != "" {
				if releaseErr := h.gatewayService.ReleaseGrokVideoBilling(ctx, videoTaskID, subject.UserID, apiKey.ID); releaseErr != nil {
					reqLog.Warn("openai_videos.video_billing_claim_release_failed",
						zap.String("request_id", videoTaskID),
						zap.Error(releaseErr),
					)
				}
			}
			logger.L().With(
				zap.String("component", "handler.openai_gateway.openai_videos"),
				zap.Int64("user_id", subject.UserID),
				zap.Int64("api_key_id", apiKey.ID),
				zap.Any("group_id", apiKey.GroupID),
				zap.String("model", requestModel),
				zap.Int64("account_id", account.ID),
			).Error("openai_videos.record_usage_failed", zap.Error(err))
			reqLog.Debug("openai_videos.record_usage_failed", zap.Error(err))
		}
	})
}
