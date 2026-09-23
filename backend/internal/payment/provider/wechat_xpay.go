package provider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// WechatXpay 实现微信「虚拟支付」(xpay)。
//
// 它面向拿不到微信支付商户号的个人主体小程序:平台按道具直购收钱,开发者不需要
// 商户号。代价是商品必须以「道具」形式在 MP 后台预先建好,单价在后台固定为「分」,
// 下单时 goodsPrice 必须与后台完全一致 —— 所以本 provider 用
// 「面额(分) → 道具ID」的映射来出单,金额对不上就报错,不会猜一个道具出来。
//
// 两套签名(见微信虚拟支付接入指引 3.4 节):
//   - paySig    = HMAC-SHA256(AppKey,     uri + "&" + postBody)  防伪校验
//   - signature = HMAC-SHA256(sessionKey, signData)              用户态签名
//
// 金额单位全程为「分」,不做任何换算(5 元 = 500 分)。
const (
	// xpayCreateSignatureURI 是 C 端(wx.requestVirtualPayment)下单签名用的固定 uri。
	xpayCreateSignatureURI = "requestVirtualPayment"
	// xpayQueryOrderURI 是 B 端查单接口的签名 uri(实际请求路径)。
	xpayQueryOrderURI = "/xpay/query_order"

	// xpayModeShortSeriesGoods 是道具直购模式。
	xpayModeShortSeriesGoods = "short_series_goods"

	xpayCurrencyType = "CNY"

	// xpayEventGoodsDeliver 是发货推送的 Event 值。
	xpayEventGoodsDeliver = "xpay_goods_deliver_notify"

	xpayDefaultTimeout = 10 * time.Second
)

// xpayAPIBase 抽成变量,便于测试替换。
var xpayAPIBase = "https://api.weixin.qq.com"

// WechatXpay 是微信虚拟支付 provider。
type WechatXpay struct {
	instanceID string
	offerID    string
	appKey     string
	env        int64
	// products 是「面额(分) → 道具ID」映射,来源为实例配置的 JSON。
	products map[int64]string
	// platform 可选,部分客户端需要 signData 带 platform 字段。
	platform string
	client   *http.Client
}

// 配置项键名。appKey 属于敏感字段,已在 providerSensitiveConfigFields 中登记。
const (
	xpayConfigOfferID  = "offerId"
	xpayConfigAppKey   = "appKey"
	xpayConfigEnv      = "env"
	xpayConfigProducts = "products"
	xpayConfigPlatform = "platform"
)

// NewWechatXpay 构造 provider。构造期即校验配置,使错误在后台保存时就暴露,
// 而不是等用户下单才失败。
func NewWechatXpay(instanceID string, config map[string]string) (*WechatXpay, error) {
	offerID := strings.TrimSpace(config[xpayConfigOfferID])
	if offerID == "" {
		return nil, infraerrors.BadRequest("XPAY_CONFIG_MISSING_KEY", "missing_required_key").
			WithMetadata(map[string]string{"key": xpayConfigOfferID})
	}

	appKey := strings.TrimSpace(config[xpayConfigAppKey])
	if appKey == "" {
		return nil, infraerrors.BadRequest("XPAY_CONFIG_MISSING_KEY", "missing_required_key").
			WithMetadata(map[string]string{"key": xpayConfigAppKey})
	}

	products, err := parseXpayProducts(config[xpayConfigProducts])
	if err != nil {
		return nil, err
	}

	env := int64(0)
	if raw := strings.TrimSpace(config[xpayConfigEnv]); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed < 0 {
			return nil, infraerrors.BadRequest("XPAY_CONFIG_INVALID_KEY", "invalid_key").
				WithMetadata(map[string]string{"key": xpayConfigEnv})
		}
		env = parsed
	}

	return &WechatXpay{
		instanceID: instanceID,
		offerID:    offerID,
		appKey:     appKey,
		env:        env,
		products:   products,
		platform:   strings.TrimSpace(config[xpayConfigPlatform]),
		client:     &http.Client{Timeout: xpayDefaultTimeout},
	}, nil
}

// parseXpayProducts 解析「面额(分) → 道具ID」映射。
// 期望形如 {"500":"product-id-1","1000":"product-id-2"}。
func parseXpayProducts(raw string) (map[int64]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, infraerrors.BadRequest("XPAY_CONFIG_MISSING_KEY", "missing_required_key").
			WithMetadata(map[string]string{"key": xpayConfigProducts})
	}

	var decoded map[string]string
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil, infraerrors.BadRequest("XPAY_CONFIG_INVALID_KEY", "invalid_key").
			WithMetadata(map[string]string{"key": xpayConfigProducts})
	}

	products := make(map[int64]string, len(decoded))
	for amountText, productID := range decoded {
		amount, err := strconv.ParseInt(strings.TrimSpace(amountText), 10, 64)
		if err != nil || amount <= 0 {
			return nil, infraerrors.BadRequest("XPAY_CONFIG_INVALID_KEY", "invalid_key").
				WithMetadata(map[string]string{"key": xpayConfigProducts, "amount": amountText})
		}

		product := strings.TrimSpace(productID)
		if product == "" {
			return nil, infraerrors.BadRequest("XPAY_CONFIG_INVALID_KEY", "invalid_key").
				WithMetadata(map[string]string{"key": xpayConfigProducts, "amount": amountText})
		}

		products[amount] = product
	}

	if len(products) == 0 {
		return nil, infraerrors.BadRequest("XPAY_CONFIG_INVALID_KEY", "invalid_key").
			WithMetadata(map[string]string{"key": xpayConfigProducts})
	}

	return products, nil
}

func (x *WechatXpay) Name() string        { return "WechatXpay" }
func (x *WechatXpay) ProviderKey() string { return payment.TypeWechatXpay }

// MerchantIdentityMetadata 暴露 OfferID 作为商户身份,供订单快照一致性校验使用。
// 实现 payment.MerchantIdentityProvider。
func (x *WechatXpay) MerchantIdentityMetadata() map[string]string {
	return map[string]string{
		"merchant_id": x.offerID,
		"currency":    xpayCurrencyType,
	}
}

func (x *WechatXpay) SupportedTypes() []payment.PaymentType {
	return []payment.PaymentType{payment.TypeWechatXpay}
}

// PresetAmounts 返回已配置道具的面额(分),用于前台展示可充值档位。
// 实现 payment.PresetAmountProvider。
func (x *WechatXpay) PresetAmounts() []int64 {
	amounts := make([]int64, 0, len(x.products))
	for amount := range x.products {
		amounts = append(amounts, amount)
	}

	return amounts
}

// CreatePayment 生成 wx.requestVirtualPayment 所需的 signData 与两套签名。
//
// 注意:此处不发起任何上游请求 —— 虚拟支付由客户端直接调起,服务端只负责出参和签名。
func (x *WechatXpay) CreatePayment(ctx context.Context, req payment.CreatePaymentRequest) (*payment.CreatePaymentResponse, error) {
	openID := strings.TrimSpace(req.OpenID)
	if openID == "" {
		return nil, infraerrors.BadRequest("XPAY_OPENID_REQUIRED", "wechat virtual payment requires the payer openid")
	}

	sessionKey := strings.TrimSpace(req.SessionKey)
	if sessionKey == "" {
		return nil, infraerrors.BadRequest("XPAY_SESSION_KEY_REQUIRED", "wechat virtual payment requires a session key")
	}

	outTradeNo := strings.TrimSpace(req.OrderID)
	if outTradeNo == "" {
		return nil, infraerrors.BadRequest("XPAY_OUT_TRADE_NO_REQUIRED", "wechat virtual payment requires a merchant order number")
	}

	goodsPrice, err := payment.YuanToFen(strings.TrimSpace(req.Amount))
	if err != nil {
		return nil, infraerrors.BadRequest("XPAY_INVALID_AMOUNT", "invalid payment amount")
	}

	productID, ok := x.products[goodsPrice]
	if !ok {
		// 金额必须精确命中已配置的道具。命中不了就让管理员去补配置(或把
		// recharge_fee_rate 调成 0,避免手续费把金额推离道具面额)。
		return nil, infraerrors.BadRequest("XPAY_PRODUCT_NOT_CONFIGURED", "no virtual payment product configured for this amount").
			WithMetadata(map[string]string{
				"goodsPrice": strconv.FormatInt(goodsPrice, 10),
				"configured": x.configuredAmountsText(),
			})
	}

	signData, err := x.buildSignData(productID, goodsPrice, outTradeNo)
	if err != nil {
		return nil, err
	}

	return &payment.CreatePaymentResponse{
		ResultType: payment.CreatePaymentResultXpayReady,
		Currency:   xpayCurrencyType,
		Xpay: &payment.WechatXpayPayload{
			Mode:       xpayModeShortSeriesGoods,
			SignData:   signData,
			PaySig:     CalcXpayPaySig(x.appKey, xpayCreateSignatureURI, signData),
			Signature:  CalcXpaySignature(sessionKey, signData),
			ProductID:  productID,
			GoodsPrice: goodsPrice,
			OutTradeNo: outTradeNo,
		},
	}, nil
}

func (x *WechatXpay) configuredAmountsText() string {
	amounts := make([]string, 0, len(x.products))
	for amount := range x.products {
		amounts = append(amounts, strconv.FormatInt(amount, 10))
	}
	return strings.Join(amounts, ",")
}

// buildSignData 组装 signData。字段清单来自虚拟支付道具直购契约:
// offerId / buyQuantity / env / currencyType / productId / goodsPrice / outTradeNo / attach。
// platform 不在该清单内,仅在实例显式配置时才带上。
func (x *WechatXpay) buildSignData(productID string, goodsPrice int64, outTradeNo string) (string, error) {
	payload := make(map[string]any, 9)
	payload["offerId"] = x.offerID
	// 每个面额对应独立道具,因此数量固定为 1。
	payload["buyQuantity"] = 1
	payload["env"] = x.env
	payload["currencyType"] = xpayCurrencyType
	payload["productId"] = productID
	payload["goodsPrice"] = goodsPrice
	payload["outTradeNo"] = outTradeNo
	// attach 必填,发货推送时原样透传;这里放业务单号便于对账。
	payload["attach"] = outTradeNo
	if x.platform != "" {
		payload["platform"] = x.platform
	}

	// 签名对象必须与实际发给 wx.requestVirtualPayment 的 signData 完全一致,
	// 因此这里不做任何格式化(不缩进、不改键顺序由 map 序列化决定后即固定)。
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal xpay sign data: %w", err)
	}

	return string(encoded), nil
}

// xpayQueryOrderRequest 是 /xpay/query_order 的请求体。
// 注意:参数名是 order_id,传的是开发者自己的业务单号 outTradeNo。
type xpayQueryOrderRequest struct {
	OpenID  string `json:"openid"`
	Env     int64  `json:"env"`
	OrderID string `json:"order_id"`
}

type xpayQueryOrderResponse struct {
	ErrCode    int64  `json:"errcode"`
	ErrMsg     string `json:"errmsg"`
	OrderID    string `json:"order_id"`
	WxOrderID  string `json:"wx_order_id"`
	Status     int64  `json:"status"`
	PaidAmount int64  `json:"paid_amount"`
	PayTime    int64  `json:"pay_time"`
}

// QueryOrder 通过 /xpay/query_order 主动查单,用于发货推送丢失时的兜底。
//
// openid 是该接口的必填参数,但 Provider 接口只传 tradeNo,因此由服务层通过
// WithXpayOpenID 注入上下文;拿不到 openid 时返回明确错误而不是发一个必失败的请求。
func (x *WechatXpay) QueryOrder(ctx context.Context, tradeNo string) (*payment.QueryOrderResponse, error) {
	openID := XpayOpenIDFromContext(ctx)
	if openID == "" {
		return nil, infraerrors.ServiceUnavailable(
			"XPAY_OPENID_UNAVAILABLE",
			"querying a virtual payment order requires the payer openid",
		)
	}

	return x.queryOrder(ctx, tradeNo, openID)
}

// queryOrder 是 /xpay/query_order 的实际调用。
// 发货推送校验也会复用它:推送本身没有可校验的签名,
// 只有带 pay_sig 的查单结果才能作为金额与状态的依据。
func (x *WechatXpay) queryOrder(ctx context.Context, orderID, openID string) (*payment.QueryOrderResponse, error) {
	body, err := json.Marshal(xpayQueryOrderRequest{
		OpenID:  strings.TrimSpace(openID),
		Env:     x.env,
		OrderID: strings.TrimSpace(orderID),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal xpay query order request: %w", err)
	}

	raw, err := x.postXpay(ctx, xpayQueryOrderURI, string(body))
	if err != nil {
		return nil, err
	}

	var decoded xpayQueryOrderResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode xpay query order response: %w", err)
	}

	if decoded.ErrCode != 0 {
		return nil, fmt.Errorf("xpay query_order errcode=%d %s", decoded.ErrCode, strings.TrimSpace(decoded.ErrMsg))
	}

	return &payment.QueryOrderResponse{
		// 平台单号以 wx_order_id 为准。
		TradeNo: strings.TrimSpace(decoded.WxOrderID),
		Status:  xpayOrderStatus(decoded.Status),
		Amount:  payment.FenToYuan(decoded.PaidAmount),
		Metadata: map[string]string{
			"xpay_order_status": strconv.FormatInt(decoded.Status, 10),
			"xpay_order_id":     strings.TrimSpace(decoded.OrderID),
		},
	}, nil
}

// xpayOrderStatus 把平台订单状态映射成 provider 层状态。
//
// ⚠️ 这套取值未在本地接入指引中给出,是按上游返回的 status 语义反推的。未知取值一律
// 落到 pending:宁可让查单兜底不生效,也不能把未经确认的订单当成已支付去发货。
// 上线前请对照官方 query_order 文档核对一次取值。
func xpayOrderStatus(status int64) string {
	switch status {
	case 1, 2:
		return payment.ProviderStatusPaid
	case 3:
		return payment.ProviderStatusRefunded
	case 4, 5:
		return payment.ProviderStatusFailed
	default:
		return payment.ProviderStatusPending
	}
}

// postXpay 调 B 端接口:pay_sig 放在 query,body 原样参与签名。
func (x *WechatXpay) postXpay(ctx context.Context, uri, body string) ([]byte, error) {
	paySig := CalcXpayPaySig(x.appKey, uri, body)
	endpoint := xpayAPIBase + uri + "?pay_sig=" + paySig

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, fmt.Errorf("build xpay request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := x.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call xpay %s: %w", uri, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read xpay %s response: %w", uri, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("xpay %s http status=%d", uri, resp.StatusCode)
	}

	return raw, nil
}

// xpayDeliverNotify 是发货推送的报文。
type xpayDeliverNotify struct {
	XMLName xml.Name `xml:"xml"`
	Event   string   `xml:"Event"`
	OpenID  string   `xml:"OpenId"`
	// OutTradeNo 是下单时我们传给微信的业务单号。
	OutTradeNo string `xml:"OutTradeNo"`
	// WeChatPayInfo.MchOrderNo 是平台单号 wx_order_id,发货与幂等以此为准。
	WeChatPayInfo struct {
		MchOrderNo string `xml:"MchOrderNo"`
	} `xml:"WeChatPayInfo"`
	GoodsInfo struct {
		ProductID string `xml:"ProductId"`
		Quantity  int64  `xml:"Quantity"`
	} `xml:"GoodsInfo"`
}

// VerifyNotification 解析发货推送。
//
// 微信发货推送不带可校验的签名头(平台靠「只有微信知道你的回调地址」保证来源),
// 也**不携带实付金额** —— 而金额校验是履约链路防伪造的唯一边界。因此这里只把推送
// 当作「唤醒」:立刻回查一次带 pay_sig 的 query_order,用平台返回的订单状态与
// paid_amount 作为唯一可信来源。伪造一条推送,最多只会触发一次查单然后被拒。
func (x *WechatXpay) VerifyNotification(ctx context.Context, rawBody string, headers map[string]string) (*payment.PaymentNotification, error) {
	var notify xpayDeliverNotify
	if err := xml.Unmarshal([]byte(rawBody), &notify); err != nil {
		return nil, fmt.Errorf("decode xpay deliver notify: %w", err)
	}

	if strings.TrimSpace(notify.Event) != xpayEventGoodsDeliver {
		// 非发货事件:返回 nil 让调用方直接回 200,避免平台重试。
		return nil, nil
	}

	outTradeNo := strings.TrimSpace(notify.OutTradeNo)
	mchOrderNo := strings.TrimSpace(notify.WeChatPayInfo.MchOrderNo)
	openID := strings.TrimSpace(notify.OpenID)
	if outTradeNo == "" || mchOrderNo == "" || openID == "" {
		return nil, fmt.Errorf("xpay deliver notify missing outTradeNo, wxOrderId or openid")
	}

	query, err := x.queryOrder(ctx, outTradeNo, openID)
	if err != nil {
		return nil, fmt.Errorf("verify xpay deliver notify via query_order: %w", err)
	}

	if query.Status != payment.ProviderStatusPaid {
		return nil, fmt.Errorf("xpay query_order reports order %s is not paid", outTradeNo)
	}

	// 双单号交叉校验:平台单号对不上说明推送与平台记录不是同一笔。
	if upstream := strings.TrimSpace(query.TradeNo); upstream != "" && upstream != mchOrderNo {
		return nil, fmt.Errorf("xpay wx_order_id mismatch: notify=%s query=%s", mchOrderNo, upstream)
	}

	return &payment.PaymentNotification{
		// TradeNo 用平台单号(发货与幂等以此为准);OrderID 用业务单号,履约按它找单。
		TradeNo: firstNonEmptyString(query.TradeNo, mchOrderNo),
		OrderID: outTradeNo,
		Amount:  query.Amount,
		Status:  payment.NotificationStatusSuccess,
		RawData: rawBody,
		Metadata: map[string]string{
			"xpay_openid":     openID,
			"xpay_product_id": strings.TrimSpace(notify.GoodsInfo.ProductID),
			"xpay_quantity":   strconv.FormatInt(notify.GoodsInfo.Quantity, 10),
		},
	}, nil
}

// firstNonEmptyString 返回第一个非空字符串。
func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}

	return ""
}

// Refund 不实现。
//
// 虚拟支付的退款接口字段未在本项目的接入指引中给出,凭空拼一个请求体风险高于收益;
// 而且 Android 退款可直接在 MP 后台【虚拟支付 → 交易订单】操作,iOS 只能由用户
// 向 App Store 申请、开发者本就无权主动退。因此这里明确报错,不提供一个会失败的入口。
func (x *WechatXpay) Refund(ctx context.Context, req payment.RefundRequest) (*payment.RefundResponse, error) {
	return nil, infraerrors.BadRequest(
		"XPAY_REFUND_UNSUPPORTED",
		"wechat virtual payment refunds are handled in the WeChat MP console",
	)
}

// CalcXpayPaySig 计算 paySig:HMAC-SHA256(AppKey, uri + "&" + postBody)。
// C 端下单时 uri 固定为 requestVirtualPayment;B 端接口传实际路径(如 /xpay/query_order)。
func CalcXpayPaySig(appKey, uri, postBody string) string {
	return hmacSHA256Hex(appKey, uri+"&"+postBody)
}

// CalcXpaySignature 计算用户态签名:HMAC-SHA256(sessionKey, signData)。
func CalcXpaySignature(sessionKey, signData string) string {
	return hmacSHA256Hex(sessionKey, signData)
}

func hmacSHA256Hex(key, message string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// --- 上下文传递 openid ---

type xpayContextKey struct{}

var xpayOpenIDKey xpayContextKey

// WithXpayOpenID 把付款人 openid 注入上下文,供 QueryOrder 使用。
func WithXpayOpenID(ctx context.Context, openID string) context.Context {
	return context.WithValue(ctx, xpayOpenIDKey, strings.TrimSpace(openID))
}

// XpayOpenIDFromContext 取出上下文中的 openid,没有则返回空串。
func XpayOpenIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(xpayOpenIDKey).(string)
	return strings.TrimSpace(value)
}
