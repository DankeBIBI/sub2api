package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/payment/provider"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 与 handler/auth_wxapp_oauth.go 保持一致的微信小程序登录态常量。
const (
	xpayMinipJscodeURL    = "https://api.weixin.qq.com/sns/jscode2session"
	xpayMinipProviderType = "wechat_minip"
	xpayMinipProviderKey  = "wxapp:openid"
	xpayMinipTimeout      = 10 * time.Second
)

// xpayCode2SessionResp 是 jscode2session 的响应。
type xpayCode2SessionResp struct {
	OpenID     string `json:"openid"`
	UnionID    string `json:"unionid"`
	SessionKey string `json:"session_key"`
	ErrCode    int64  `json:"errcode"`
	ErrMsg     string `json:"errmsg"`
}

// resolveXpayPayer 用小程序 uni.login 拿到的 jsCode 换 openid + session_key。
//
// 为什么在支付时现换,而不是登录时把 session_key 存库:
//   - signature 必须用 session_key 签,而每次 code2Session 都会轮换 session_key,
//     存库反而容易拿到已经失效的那个;
//   - 现换现用不需要新增存储,也不让长期保存的 session_key 变成额外的泄露面。
func (s *PaymentService) resolveXpayPayer(ctx context.Context, jsCode string) (string, string, error) {
	code := strings.TrimSpace(jsCode)
	if code == "" {
		return "", "", infraerrors.BadRequest(
			"XPAY_JSCODE_REQUIRED",
			"wechat virtual payment requires a fresh mini program login code",
		)
	}

	if s == nil || s.configService == nil || s.configService.settingRepo == nil {
		return "", "", infraerrors.ServiceUnavailable(
			"XPAY_MINIP_NOT_CONFIGURED",
			"wechat virtual payment requires the mini program appid and secret",
		)
	}

	appID, appSecret, err := (&SettingService{settingRepo: s.configService.settingRepo}).
		GetWechatMinipConfig(ctx)
	if err != nil || strings.TrimSpace(appID) == "" || strings.TrimSpace(appSecret) == "" {
		return "", "", infraerrors.ServiceUnavailable(
			"XPAY_MINIP_NOT_CONFIGURED",
			"wechat virtual payment requires the mini program appid and secret",
		)
	}

	session, err := exchangeXpayJscode(ctx, appID, appSecret, code)
	if err != nil {
		return "", "", err
	}

	openID := strings.TrimSpace(session.OpenID)
	if openID == "" {
		return "", "", infraerrors.Unauthorized("XPAY_NO_OPENID", "jscode2session returned no openid")
	}

	sessionKey := strings.TrimSpace(session.SessionKey)
	if sessionKey == "" {
		return "", "", infraerrors.Unauthorized("XPAY_NO_SESSION_KEY", "jscode2session returned no session key")
	}

	return openID, sessionKey, nil
}

// exchangeXpayJscode 调微信 jscode2session。
func exchangeXpayJscode(ctx context.Context, appID, appSecret, code string) (*xpayCode2SessionResp, error) {
	endpoint, err := url.Parse(xpayMinipJscodeURL)
	if err != nil {
		return nil, fmt.Errorf("parse jscode2session url: %w", err)
	}

	query := endpoint.Query()
	query.Set("appid", appID)
	query.Set("secret", appSecret)
	query.Set("js_code", code)
	query.Set("grant_type", "authorization_code")
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build jscode2session request: %w", err)
	}

	client := &http.Client{Timeout: xpayMinipTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, infraerrors.Unauthorized("XPAY_CODE_INVALID", "call jscode2session failed").WithCause(err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, infraerrors.Unauthorized("XPAY_CODE_INVALID", "read jscode2session body failed").WithCause(err)
	}

	var out xpayCode2SessionResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, infraerrors.Unauthorized("XPAY_CODE_INVALID", "decode jscode2session body failed").WithCause(err)
	}

	if out.ErrCode != 0 {
		return nil, infraerrors.Unauthorized(
			"XPAY_CODE_INVALID",
			fmt.Sprintf("jscode2session errcode=%d %s", out.ErrCode, strings.TrimSpace(out.ErrMsg)),
		)
	}

	return &out, nil
}

// xpayPayerOpenID 从 auth_identities 取该用户的小程序 openid。
// 用于查单兜底:/xpay/query_order 必须带 openid,而 Provider 接口只传单号。
func (s *PaymentService) xpayPayerOpenID(ctx context.Context, userID int64) string {
	if s == nil || s.entClient == nil || userID <= 0 {
		return ""
	}

	// 用 First 而非 Only:同一用户理论上只该有一条绑定,但历史数据可能重复,
	// Only 遇到多行会直接报错、让兜底静默失效。这里只是兜底路径,取一条即可。
	identity, err := s.entClient.AuthIdentity.Query().
		Where(
			authidentity.UserIDEQ(userID),
			authidentity.ProviderTypeEQ(xpayMinipProviderType),
			authidentity.ProviderKeyEQ(xpayMinipProviderKey),
		).
		First(ctx)
	if err != nil || identity == nil {
		return ""
	}

	return strings.TrimSpace(identity.ProviderSubject)
}

// providerQueryContext 在查单前把渠道额外需要的上下文注入进去。
// 目前只有微信虚拟支付需要在上下文里带 openid(它的 /xpay/query_order 必填该参数,
// 而 Provider 接口只传单号)。
func providerQueryContext(ctx context.Context, s *PaymentService, order *dbent.PaymentOrder) context.Context {
	return s.withXpayQueryContext(ctx, order)
}

// withXpayQueryContext 查单前把 openid 注入上下文,供 provider.QueryOrder 使用。
//
// 优先用订单快照里下单当时记下的 openid —— 它才是真正的付款人;
// auth_identities 只作兜底(比如换微信号重新绑定过的历史订单)。
// 两种来源都拿不到时原样返回,由 provider 给出明确错误,而不是发一个必失败的请求。
func (s *PaymentService) withXpayQueryContext(ctx context.Context, order *dbent.PaymentOrder) context.Context {
	if order == nil {
		return ctx
	}

	providerKey := strings.TrimSpace(psStringValue(order.ProviderKey))
	if providerKey == "" && order.PaymentType != "" {
		providerKey = strings.TrimSpace(order.PaymentType)
	}

	if providerKey != payment.TypeWechatXpay {
		return ctx
	}

	openID := ""
	if snapshot := psOrderProviderSnapshot(order); snapshot != nil {
		openID = strings.TrimSpace(snapshot.XpayOpenID)
	}
	if openID == "" {
		openID = s.xpayPayerOpenID(ctx, order.UserID)
	}
	if openID == "" {
		return ctx
	}

	return provider.WithXpayOpenID(ctx, openID)
}
