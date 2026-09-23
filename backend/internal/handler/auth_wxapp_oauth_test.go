package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBuildWxAppVirtualEmail_Format 验证合成邮箱格式
func TestBuildWxAppVirtualEmail_Format(t *testing.T) {
	unionid := "oKX2x4u1AbCdEf123_-"
	email := buildWxAppVirtualEmail(unionid)

	// 应全部小写
	require.Equal(t, strings.ToLower(email), email, "synthetic email should be all lowercase")
	// 前缀正确
	require.True(t, strings.HasPrefix(email, "wxapp-"), "should have wxapp- prefix")
	// 后缀是 RFC 保留 .invalid 域名
	require.True(t, strings.HasSuffix(email, "@sub2api-wxapp-connect.invalid"), "should have reserved .invalid domain")
	// 不应包含 unionid 原大小写(已转小写)
	require.NotContains(t, email, "AbCdEf", "unionid should be lowercased")
}

// TestBuildWxAppVirtualEmail_EmptyIdentity 处理边界
func TestBuildWxAppVirtualEmail_EmptyIdentity(t *testing.T) {
	email := buildWxAppVirtualEmail("")
	// 空标识时不 panic,且仍然返回合法结构(只是标识段为空)
	require.Equal(t, "wxapp-@sub2api-wxapp-connect.invalid", email)
}

// TestWechatMinipIdentityRe_ValidCases 验证 openid 正则的白名单放行
func TestWechatMinipIdentityRe_ValidCases(t *testing.T) {
	cases := []string{
		"oKX2x4u1AbCdEf123", // 16 字符(微信真实长度区间)
		"aB_-9X9z",          // 最短 8
		"a_b-c-d_e-f",       // 混合下划线/连字符
		"oKX2x4u1AbCdEf1234aB_-9X9zY" + "xX9zY9xX9zY9xX9zY9xX9zY9xX9zY9xX", // 长串(28 字符级)
	}
	for _, c := range cases {
		require.True(t, wechatMinipIdentityRe.MatchString(c), "should accept: %s", c)
	}
}

// TestWechatMinipIdentityRe_InvalidCases 验证 openid 正则拒绝非法输入
func TestWechatMinipIdentityRe_InvalidCases(t *testing.T) {
	cases := []string{
		"",      // 空
		"short", // 长度 < 8
		"oKX2x4u1AbCdEf123-oKX2x4u1AbCdEf123-oKX2x4u1AbCdEf123-xX9zY9xX9zY9xX9zY9xX9zY9xX9zY9x", // 长度 > 64
		"has space in middle", // 含空格
		"with/slash",          // 含斜杠
		"with@symbol",         // 含 @
		"中文字符",                // 非 ASCII
		"emoji_😀",             // emoji
	}
	for _, c := range cases {
		require.False(t, wechatMinipIdentityRe.MatchString(c), "should reject: %q", c)
	}
}

// TestFirstN_Basic 验证字符串截断
func TestFirstN_Basic(t *testing.T) {
	require.Equal(t, "abc", firstN("abcdef", 3))
	require.Equal(t, "abcdef", firstN("abcdef", 10)) // n 超长
	require.Equal(t, "abc", firstN("abc", 5))        // 等长
	require.Equal(t, "", firstN("", 5))              // 空
}

// TestFirstN_Unicode 验证截断不会越过字符串边界
func TestFirstN_Unicode(t *testing.T) {
	// 截断到 4 字节范围(简单截字节;username 是 wx_+openid 片段,openid 是 ASCII)
	require.Equal(t, "abc", firstN("abc中文", 3))
}

// stubWxAppCode2Session 仅为编译占位,实际 mock 见 WxAppLogin handler 集成测试
var _ = context.Background
