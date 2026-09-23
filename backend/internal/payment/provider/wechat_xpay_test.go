package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

// testXpayDeliverXML 是一条格式完整的发货推送。
const testXpayDeliverXML = `<xml>
	<Event>xpay_goods_deliver_notify</Event>
	<OpenId>oTestOpenID1234567890</OpenId>
	<OutTradeNo>T20260913001</OutTradeNo>
	<WeChatPayInfo><MchOrderNo>wx1234567890</MchOrderNo></WeChatPayInfo>
	<GoodsInfo><ProductId>pid-5</ProductId><Quantity>1</Quantity></GoodsInfo>
</xml>`

// stubXpayQuery 把上游指向本地 stub,使测试不依赖真实微信接口。
func stubXpayQuery(t *testing.T, payload map[string]any) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != xpayQueryOrderURI {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}

		// B 端接口必须带 pay_sig,否则上游会拒绝
		if r.URL.Query().Get("pay_sig") == "" {
			t.Error("query_order request is missing pay_sig")
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Errorf("encode stub response: %v", err)
		}
	}))

	previous := xpayAPIBase
	xpayAPIBase = server.URL
	t.Cleanup(func() {
		xpayAPIBase = previous
		server.Close()
	})
}

const (
	testXpayOfferID = "1450000000"
	testXpayAppKey  = "test-app-key"
	testXpaySession = "test-session-key"
	testXpayOpenID  = "oTestOpenID1234567890"
)

func newTestXpay(t *testing.T) *WechatXpay {
	t.Helper()

	prov, err := NewWechatXpay("inst-1", map[string]string{
		xpayConfigOfferID:  testXpayOfferID,
		xpayConfigAppKey:   testXpayAppKey,
		xpayConfigProducts: `{"500":"pid-5","1000":"pid-10","2000":"pid-20","5000":"pid-50","10000":"pid-100"}`,
	})
	if err != nil {
		t.Fatalf("NewWechatXpay: %v", err)
	}

	return prov
}

func TestNewWechatXpayRejectsIncompleteConfig(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]string
	}{
		{"missing offerId", map[string]string{xpayConfigAppKey: "k", xpayConfigProducts: `{"500":"p"}`}},
		{"missing appKey", map[string]string{xpayConfigOfferID: "o", xpayConfigProducts: `{"500":"p"}`}},
		{"missing products", map[string]string{xpayConfigOfferID: "o", xpayConfigAppKey: "k"}},
		{"products not json", map[string]string{xpayConfigOfferID: "o", xpayConfigAppKey: "k", xpayConfigProducts: "500=pid"}},
		{"products empty object", map[string]string{xpayConfigOfferID: "o", xpayConfigAppKey: "k", xpayConfigProducts: `{}`}},
		{"products bad amount", map[string]string{xpayConfigOfferID: "o", xpayConfigAppKey: "k", xpayConfigProducts: `{"0":"p"}`}},
		{"products blank id", map[string]string{xpayConfigOfferID: "o", xpayConfigAppKey: "k", xpayConfigProducts: `{"500":" "}`}},
		{"env not a number", map[string]string{xpayConfigOfferID: "o", xpayConfigAppKey: "k", xpayConfigProducts: `{"500":"p"}`, xpayConfigEnv: "abc"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewWechatXpay("inst", tc.config); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestNewWechatXpayDefaultsEnvToZero(t *testing.T) {
	prov := newTestXpay(t)
	if prov.env != 0 {
		t.Fatalf("env = %d, want 0", prov.env)
	}

	if got := prov.ProviderKey(); got != payment.TypeWechatXpay {
		t.Fatalf("ProviderKey() = %q", got)
	}

	types := prov.SupportedTypes()
	if len(types) != 1 || types[0] != payment.TypeWechatXpay {
		t.Fatalf("SupportedTypes() = %v", types)
	}
}

func TestWechatXpayCreatePaymentSignsSignData(t *testing.T) {
	prov := newTestXpay(t)

	resp, err := prov.CreatePayment(context.Background(), payment.CreatePaymentRequest{
		OrderID:     "T20260913001",
		Amount:      "5.00",
		PaymentType: payment.TypeWechatXpay,
		OpenID:      testXpayOpenID,
		SessionKey:  testXpaySession,
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}

	if resp.ResultType != payment.CreatePaymentResultXpayReady {
		t.Fatalf("ResultType = %q", resp.ResultType)
	}

	if resp.Xpay == nil {
		t.Fatal("Xpay payload is nil")
	}

	xpay := resp.Xpay
	if xpay.Mode != xpayModeShortSeriesGoods {
		t.Fatalf("Mode = %q", xpay.Mode)
	}

	if xpay.GoodsPrice != 500 {
		t.Fatalf("GoodsPrice = %d, want 500", xpay.GoodsPrice)
	}

	if xpay.ProductID != "pid-5" {
		t.Fatalf("ProductID = %q, want pid-5", xpay.ProductID)
	}

	// signData 必须与发给 wx.requestVirtualPayment 的完全一致,因此逐字段核对。
	var decoded map[string]any
	if err := json.Unmarshal([]byte(xpay.SignData), &decoded); err != nil {
		t.Fatalf("signData is not valid json: %v", err)
	}

	wantFields := map[string]any{
		"offerId":      testXpayOfferID,
		"buyQuantity":  float64(1),
		"env":          float64(0),
		"currencyType": "CNY",
		"productId":    "pid-5",
		"goodsPrice":   float64(500),
		"outTradeNo":   "T20260913001",
		"attach":       "T20260913001",
	}
	for key, want := range wantFields {
		if got := decoded[key]; got != want {
			t.Errorf("signData[%s] = %v, want %v", key, got, want)
		}
	}

	if len(decoded) != len(wantFields) {
		t.Errorf("signData has %d fields, want %d: %s", len(decoded), len(wantFields), xpay.SignData)
	}

	// paySig 与 signature 用独立实现复算,避免只用被测函数验证被测函数。
	wantPaySig := independentHMAC(testXpayAppKey, xpayCreateSignatureURI+"&"+xpay.SignData)
	if xpay.PaySig != wantPaySig {
		t.Errorf("PaySig = %q, want %q", xpay.PaySig, wantPaySig)
	}

	wantSignature := independentHMAC(testXpaySession, xpay.SignData)
	if xpay.Signature != wantSignature {
		t.Errorf("Signature = %q, want %q", xpay.Signature, wantSignature)
	}
}

func TestWechatXpayCreatePaymentRejectsUnconfiguredAmount(t *testing.T) {
	prov := newTestXpay(t)

	_, err := prov.CreatePayment(context.Background(), payment.CreatePaymentRequest{
		OrderID:     "T20260913002",
		Amount:      "6.00",
		PaymentType: payment.TypeWechatXpay,
		OpenID:      testXpayOpenID,
		SessionKey:  testXpaySession,
	})
	if err == nil {
		t.Fatal("expected error for unconfigured amount")
	}

	if !strings.Contains(err.Error(), "no virtual payment product configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWechatXpayCreatePaymentRequiresPayerContext(t *testing.T) {
	prov := newTestXpay(t)

	cases := []struct {
		name string
		req  payment.CreatePaymentRequest
	}{
		{"no openid", payment.CreatePaymentRequest{OrderID: "T1", Amount: "5.00", SessionKey: testXpaySession}},
		{"no session key", payment.CreatePaymentRequest{OrderID: "T1", Amount: "5.00", OpenID: testXpayOpenID}},
		{"no order id", payment.CreatePaymentRequest{Amount: "5.00", OpenID: testXpayOpenID, SessionKey: testXpaySession}},
		{"bad amount", payment.CreatePaymentRequest{OrderID: "T1", Amount: "abc", OpenID: testXpayOpenID, SessionKey: testXpaySession}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := prov.CreatePayment(context.Background(), tc.req); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestXpayQueryOrderMapsUpstreamResult(t *testing.T) {
	prov := newTestXpay(t)
	stubXpayQuery(t, map[string]any{
		"errcode": 0, "order_id": "T20260913001", "wx_order_id": "wx1234567890", "status": 2, "paid_amount": 1000,
	})

	ctx := WithXpayOpenID(context.Background(), testXpayOpenID)

	resp, err := prov.QueryOrder(ctx, "T20260913001")
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}

	if resp.Status != payment.ProviderStatusPaid {
		t.Errorf("Status = %q", resp.Status)
	}

	// 金额必须换算成元(penny → yuan),履约链路拿它和订单金额比对
	if resp.Amount != 10 {
		t.Errorf("Amount = %v, want 10", resp.Amount)
	}

	if resp.TradeNo != "wx1234567890" {
		t.Errorf("TradeNo = %q", resp.TradeNo)
	}
}

func TestXpayQueryOrderSurfacesUpstreamError(t *testing.T) {
	prov := newTestXpay(t)
	stubXpayQuery(t, map[string]any{"errcode": 9300001, "errmsg": "invalid order"})

	ctx := WithXpayOpenID(context.Background(), testXpayOpenID)
	if _, err := prov.QueryOrder(ctx, "T20260913001"); err == nil {
		t.Fatal("expected upstream errcode to be surfaced")
	}
}

func TestWechatXpayVerifyDeliverNotification(t *testing.T) {
	prov := newTestXpay(t)
	stubXpayQuery(t, map[string]any{
		"errcode": 0, "order_id": "T20260913001", "wx_order_id": "wx1234567890", "status": 1, "paid_amount": 500,
	})

	notification, err := prov.VerifyNotification(context.Background(), testXpayDeliverXML, nil)
	if err != nil {
		t.Fatalf("VerifyNotification: %v", err)
	}

	if notification == nil {
		t.Fatal("notification is nil")
	}

	if notification.OrderID != "T20260913001" {
		t.Errorf("OrderID = %q, want T20260913001", notification.OrderID)
	}

	// 平台单号 wx_order_id 是发货与幂等去重的依据。
	if notification.TradeNo != "wx1234567890" {
		t.Errorf("TradeNo = %q, want wx1234567890", notification.TradeNo)
	}

	// 金额只能来自带 pay_sig 的查单结果 —— 它是履约链路防伪造推送的唯一屏障。
	if notification.Amount != 5 {
		t.Errorf("Amount = %v, want 5", notification.Amount)
	}

	if notification.Status != payment.NotificationStatusSuccess {
		t.Errorf("Status = %q", notification.Status)
	}

	if notification.Metadata["xpay_product_id"] != "pid-5" {
		t.Errorf("productId metadata = %q", notification.Metadata["xpay_product_id"])
	}
}

func TestWechatXpayVerifyDeliverNotificationRejectsForgedPush(t *testing.T) {
	prov := newTestXpay(t)
	// 平台侧显示未支付:伪造推送不能触发发货
	stubXpayQuery(t, map[string]any{
		"errcode": 0, "order_id": "T20260913001", "status": 0, "paid_amount": 0,
	})

	notification, err := prov.VerifyNotification(context.Background(), testXpayDeliverXML, nil)
	if err == nil {
		t.Fatal("expected forged deliver notify to be rejected")
	}

	if notification != nil {
		t.Fatalf("expected nil notification, got %+v", notification)
	}
}

func TestWechatXpayVerifyDeliverNotificationRejectsTradeNoMismatch(t *testing.T) {
	prov := newTestXpay(t)
	stubXpayQuery(t, map[string]any{
		"errcode": 0, "order_id": "T20260913001", "wx_order_id": "wx-someone-else", "status": 1, "paid_amount": 500,
	})

	if _, err := prov.VerifyNotification(context.Background(), testXpayDeliverXML, nil); err == nil {
		t.Fatal("expected wx_order_id mismatch to be rejected")
	}
}

func TestWechatXpayVerifyDeliverNotificationFailsWhenQueryFails(t *testing.T) {
	prov := newTestXpay(t)
	stubXpayQuery(t, map[string]any{"errcode": 9300001, "errmsg": "invalid order"})

	if _, err := prov.VerifyNotification(context.Background(), testXpayDeliverXML, nil); err == nil {
		t.Fatal("expected upstream error to be surfaced")
	}
}

func TestWechatXpayVerifyNotificationIgnoresOtherEvents(t *testing.T) {
	prov := newTestXpay(t)

	notification, err := prov.VerifyNotification(
		context.Background(),
		`<xml><Event>xpay_refund_notify</Event><OutTradeNo>T1</OutTradeNo></xml>`,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if notification != nil {
		t.Fatalf("expected nil notification for unrelated event, got %+v", notification)
	}
}

func TestWechatXpayVerifyNotificationRejectsMalformed(t *testing.T) {
	prov := newTestXpay(t)

	cases := []string{
		`not xml at all`,
		`<xml><Event>xpay_goods_deliver_notify</Event></xml>`,
		`<xml><Event>xpay_goods_deliver_notify</Event><OutTradeNo>T1</OutTradeNo></xml>`,
	}

	for _, body := range cases {
		if _, err := prov.VerifyNotification(context.Background(), body, nil); err == nil {
			t.Errorf("expected error for body %q", body)
		}
	}
}

func TestXpayOrderStatusMapping(t *testing.T) {
	cases := map[int64]string{
		0: payment.ProviderStatusPending,
		1: payment.ProviderStatusPaid,
		2: payment.ProviderStatusPaid,
		3: payment.ProviderStatusRefunded,
		4: payment.ProviderStatusFailed,
		5: payment.ProviderStatusFailed,
		9: payment.ProviderStatusPending,
	}

	for status, want := range cases {
		if got := xpayOrderStatus(status); got != want {
			t.Errorf("xpayOrderStatus(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestXpayQueryOrderWithoutOpenIDFailsLoudly(t *testing.T) {
	prov := newTestXpay(t)

	_, err := prov.QueryOrder(context.Background(), "T20260913001")
	if err == nil {
		t.Fatal("expected error when openid is absent from context")
	}

	if !strings.Contains(err.Error(), "requires the payer openid") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestXpayOpenIDContextRoundTrip(t *testing.T) {
	if got := XpayOpenIDFromContext(context.Background()); got != "" {
		t.Fatalf("empty context should return empty openid, got %q", got)
	}

	ctx := WithXpayOpenID(context.Background(), "  "+testXpayOpenID+"  ")
	if got := XpayOpenIDFromContext(ctx); got != testXpayOpenID {
		t.Fatalf("round trip = %q", got)
	}
}

func TestWechatXpayRefundIsExplicitlyUnsupported(t *testing.T) {
	prov := newTestXpay(t)

	_, err := prov.Refund(context.Background(), payment.RefundRequest{TradeNo: "wx1", OrderID: "T1", Amount: "5.00"})
	if err == nil {
		t.Fatal("expected refund to be unsupported")
	}

	if !strings.Contains(err.Error(), "refunds are handled in the WeChat MP console") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWechatXpaySatisfiesProviderInterface(t *testing.T) {
	var _ payment.Provider = (*WechatXpay)(nil)
}

func TestWechatXpayPresetAmountsExposeConfiguredDenominations(t *testing.T) {
	prov := newTestXpay(t)

	// 服务层通过这个可选接口拿档位,不依赖具体类型。
	var preset payment.PresetAmountProvider = prov

	got := preset.PresetAmounts()
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })

	want := []int64{500, 1000, 2000, 5000, 10000}
	if len(got) != len(want) {
		t.Fatalf("PresetAmounts() = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PresetAmounts() = %v, want %v", got, want)
		}
	}
}

// independentHMAC 用标准库直接复算签名,与被测的 CalcXpayPaySig/CalcXpaySignature 无共享代码。
func independentHMAC(key, message string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}
