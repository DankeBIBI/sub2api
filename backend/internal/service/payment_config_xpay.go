package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"

	"github.com/Wei-Shaw/sub2api/ent/paymentproviderinstance"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/payment/provider"
)

// GetXpayPresetAmounts 汇总所有已启用的微信虚拟支付实例里配置好的道具面额。
//
// 虚拟支付只能按 MP 后台预先建好的「道具」售卖,所以前台可选的充值档位就等于
// 这里返回的面额:没配道具的档位不会出现在结果里,前端据此隐藏,而不是等用户
// 点了才报 XPAY_PRODUCT_NOT_CONFIGURED。
//
// 返回值是按数值升序排列的金额字符串(元),如 ["5.00","10.00","20.00"]。
func (s *PaymentConfigService) GetXpayPresetAmounts(ctx context.Context) ([]string, error) {
	if s == nil || s.entClient == nil {
		return []string{}, nil
	}

	cfg, err := s.GetPaymentConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("get payment config: %w", err)
	}

	// 虚拟支付的道具单价由 MP 后台固定,任何手续费都会把实付金额推离道具价，
	// 导致下单必定失败。这里直接返回空档位,前端就不会展示点了也付不了的入口。
	if cfg.RechargeFeeRate > 0 {
		slog.Warn("xpay preset amounts suppressed by non-zero recharge fee rate",
			"recharge_fee_rate", cfg.RechargeFeeRate)
		return []string{}, nil
	}

	instances, err := s.entClient.PaymentProviderInstance.Query().
		Where(
			paymentproviderinstance.EnabledEQ(true),
			paymentproviderinstance.ProviderKeyEQ(payment.TypeWechatXpay),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query xpay provider instances: %w", err)
	}

	seen := make(map[int64]struct{})
	for _, inst := range instances {
		cfg, err := s.decryptConfig(inst.Config)
		if err != nil {
			slog.Warn("decrypt xpay provider config failed", "instance", inst.ID, "error", err)
			continue
		}

		prov, err := provider.CreateProvider(
			payment.TypeWechatXpay,
			strconv.FormatInt(int64(inst.ID), 10),
			cfg,
		)
		if err != nil {
			// 配置填一半的实例(如缺 appKey)在这里被跳过,不影响其它实例的档位。
			slog.Warn("build xpay provider failed", "instance", inst.ID, "error", err)
			continue
		}

		preset, ok := prov.(payment.PresetAmountProvider)
		if !ok {
			continue
		}

		for _, amount := range preset.PresetAmounts() {
			if amount > 0 {
				seen[amount] = struct{}{}
			}
		}
	}

	ordered := make([]int64, 0, len(seen))
	for amount := range seen {
		ordered = append(ordered, amount)
	}

	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })

	amounts := make([]string, 0, len(ordered))
	for _, amount := range ordered {
		amounts = append(amounts, strconv.FormatFloat(payment.FenToYuan(amount), 'f', 2, 64))
	}

	return amounts, nil
}
