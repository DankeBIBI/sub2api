package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"

	"github.com/gin-gonic/gin"
)

const (
	WechatMinipProviderType = "wechat_minip"
	// WechatMinipProviderKey 身份定位键:用 openid 而非 unionid。
	// openid 在「同一小程序 + 同一用户」下终生稳定;unionid 仅在小程序绑定过微信开放平台
	// 账号时才有(个人主体无法绑定),不能作为登录前提。
	WechatMinipProviderKey = "wxapp:openid"
	// WechatMinipVirtualDomain 微信小程序合成邮箱专用域(RFC 2606 保留 .invalid TLD)
	// 跟项目内 linuxdo-connect.invalid / wechat-connect.invalid / dingtalk-connect.invalid 保持一致
	WechatMinipVirtualDomain = "sub2api-wxapp-connect.invalid"
	WechatMinipEmailPrefix   = "wxapp-"
	WechatMinipJscodeURL     = "https://api.weixin.qq.com/sns/jscode2session"
)

// wechatMinipIdentityRe 校验微信 openid/unionid 形态(都是 [A-Za-z0-9_-],长度 8~64)
var wechatMinipIdentityRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

type WxAppLoginRequest struct {
	Code string `json:"code" binding:"required"`
}

type wxAppCode2SessionResp struct {
	OpenID     string `json:"openid"`
	UnionID    string `json:"unionid"`
	SessionKey string `json:"session_key"`
	ErrCode    int64  `json:"errcode"`
	ErrMsg     string `json:"errmsg"`
}

// WxAppLogin 小程序一键登录/注册入口
// POST /api/v1/auth/wxapp/login
func (h *AuthHandler) WxAppLogin(c *gin.Context) {
	// 0. 独立开关
	// if h.settingSvc == nil || !h.settingSvc.IsWechatMinipEnabled(c.Request.Context()) {
	// 	response.ErrorFrom(c, infraerrors.Forbidden("WXAPP_DISABLED", "wechat minip login is disabled"))
	// 	return
	// }

	var req WxAppLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	// 1. 读 admin 配置的 appid/secret
	appID, appSecret, err := h.settingSvc.GetWechatMinipConfig(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// 2. code 换 openid(+ 可能的 unionid)
	session, err := exchangeWxAppCode(c.Request.Context(), appID, appSecret, strings.TrimSpace(req.Code))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// 身份主键用 openid:它在「同一小程序 + 同一用户」下终生稳定,且不依赖任何平台绑定。
	// unionid 只有小程序绑定过微信开放平台账号才返回(个人主体小程序无法绑定,
	// 300 元/年认证费也只对企业主体开放),所以不能拿它当登录前提;
	// 有就一并记进 auth_identity 供将来跨端打通,没有也不影响登录。
	openid := strings.TrimSpace(session.OpenID)
	if openid == "" {
		response.ErrorFrom(c, infraerrors.Unauthorized("WXAPP_NO_OPENID", "jscode2session returned no openid"))
		return
	}
	if !wechatMinipIdentityRe.MatchString(openid) {
		response.ErrorFrom(c, infraerrors.BadRequest("WXAPP_BAD_OPENID", "invalid openid format"))
		return
	}
	unionid := strings.TrimSpace(session.UnionID)

	// 3. 拼虚拟邮箱(作为 sub2api 内部唯一标识),复用现有 OAuth 登录/注册流程
	//    LoginOrRegisterOAuthWithTokenPair 内部:
	//    - GetByEmail 查用户
	//    - 不存在则 CreateWithEmailAliasGuard 串行化创建
	//    - randomHexString(32) 生成密码哈希
	//    - signupSource="wechat_minip" 走独立赠额配置(若 admin 后台配了)
	virtualEmail := buildWxAppVirtualEmail(openid)
	username := "wx_" + firstN(openid, 12)

	// 3.5 BackendMode 守卫:admin-only 模式下禁止通过 OAuth 新建用户
	if err := h.ensureBackendModeAllowsNewUserLogin(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	tokenPair, user, err := h.authService.LoginOrRegisterOAuthWithTokenPair(
		c.Request.Context(),
		virtualEmail,
		username,
		"",                      // invitationCode:小程序流程不强制邀请码
		"",                      // affiliateCode
		WechatMinipProviderType, // signupSource = "wechat_minip"
	)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// 4. 写一条 auth_identity(为以后 unionid 跨端打通预留;失败不回滚 user,OnConflictColumns 幂等;
	//    真实 DB 故障用 slog.Warn 告警,不阻断登录)
	if err := bindWechatMinipIdentity(c.Request.Context(), h.entClient(), user.ID, appID, openid, unionid); err != nil {
		slog.WarnContext(c.Request.Context(), "bind wechat minip identity failed",
			"user_id", user.ID,
			"app_id", appID,
			"error", err)
	}

	// 4.5 刷新 LastLoginAt 等活跃度字段(与其他 OAuth 渠道保持一致)
	h.authService.RecordSuccessfulLogin(c.Request.Context(), user.ID)

	// 5. 颁 token(与现有 OAuth 渠道统一响应体)
	response.Success(c, gin.H{
		"access_token":  tokenPair.AccessToken,
		"refresh_token": tokenPair.RefreshToken,
		"expires_in":    tokenPair.ExpiresIn,
		"token_type":    "Bearer",
		"user":          dto.UserFromService(user),
	})
}

// exchangeWxAppCode 用 code 调微信 jscode2session 换 openid + unionid + session_key
func exchangeWxAppCode(ctx context.Context, appID, appSecret, code string) (*wxAppCode2SessionResp, error) {
	if code == "" {
		return nil, infraerrors.BadRequest("WXAPP_CODE_MISSING", "code is required")
	}

	endpoint, err := url.Parse(WechatMinipJscodeURL)
	if err != nil {
		return nil, infraerrors.InternalServer("WXAPP_URL_PARSE_FAILED", "parse jscode2session url failed").WithCause(err)
	}
	query := endpoint.Query()
	query.Set("appid", appID)
	query.Set("secret", appSecret)
	query.Set("js_code", code)
	query.Set("grant_type", "authorization_code")
	endpoint.RawQuery = query.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, infraerrors.InternalServer("WXAPP_REQUEST_BUILD_FAILED", "build jscode2session request failed").WithCause(err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, infraerrors.Unauthorized("WXAPP_CODE_INVALID", "call jscode2session failed").WithCause(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, infraerrors.Unauthorized("WXAPP_CODE_INVALID", "read jscode2session body failed").WithCause(err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, infraerrors.Unauthorized("WXAPP_CODE_INVALID", fmt.Sprintf("jscode2session http status=%d", resp.StatusCode))
	}

	var out wxAppCode2SessionResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, infraerrors.Unauthorized("WXAPP_CODE_INVALID", "decode jscode2session body failed").WithCause(err)
	}
	if out.ErrCode != 0 {
		return nil, infraerrors.Unauthorized("WXAPP_CODE_INVALID", fmt.Sprintf("jscode2session errcode=%d %s", out.ErrCode, strings.TrimSpace(out.ErrMsg)))
	}
	return &out, nil
}

// bindWechatMinipIdentity 写 auth_identity 记录(幂等,失败回滚 user 不必要)
// provider_subject 用 openid(稳定身份);unionid 可能为空,只作为元信息留存。
func bindWechatMinipIdentity(ctx context.Context, client *dbent.Client, userID int64, appID, openid, unionid string) error {
	if client == nil {
		return fmt.Errorf("ent client is nil")
	}
	return client.AuthIdentity.Create().
		SetUserID(userID).
		SetProviderType(WechatMinipProviderType).
		SetProviderKey(WechatMinipProviderKey).
		SetProviderSubject(openid).
		SetIssuer(appID).
		SetMetadata(map[string]any{
			"openid":  openid,
			"unionid": unionid, // 可能为空:小程序未绑定微信开放平台
			"app_id":  appID,
			"channel": "wechat_minip",
		}).
		OnConflictColumns(
			authidentity.FieldProviderType,
			authidentity.FieldProviderKey,
			authidentity.FieldProviderSubject,
		).
		DoNothing().
		Exec(ctx)
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// buildWxAppVirtualEmail 把身份标识(openid)拼成 sub2api 内部虚拟邮箱(唯一标识)
// 全部小写,跟 isReservedEmail/ExistsByEmailAlias 等归一化逻辑一致
func buildWxAppVirtualEmail(identity string) string {
	return WechatMinipEmailPrefix + strings.ToLower(strings.TrimSpace(identity)) + "@" + WechatMinipVirtualDomain
}
