package mail

import "testing"

func TestDetectOTP(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		text    string
		html    string
		want    string
	}{
		{
			name:    "chinese subject only",
			subject: "【某服务】验证码 123456",
			want:    "123456",
		},
		{
			name: "chinese body with colon",
			text: "您好，您的验证码是：845201，请在 10 分钟内使用。",
			want: "845201",
		},
		{
			name: "english body",
			text: "Your verification code is 902134. It expires in 5 minutes.",
			want: "902134",
		},
		{
			name: "code precedes keyword",
			text: "551204 is your one-time password. Do not share it.",
			want: "551204",
		},
		{
			// The parser's bluemonday fallback glues the cells into "12345610",
			// so DetectOTP must flatten the HTML itself.
			name: "table cells are not merged",
			text: "验证码12345610分钟内有效",
			html: "<table><tr><td>验证码</td><td>123456</td><td>10分钟</td></tr></table>",
			want: "123456",
		},
		{
			name: "html without plain text",
			html: "<div><p>动态密码</p><p><strong>778291</strong></p><p>请勿泄露</p></div>",
			want: "778291",
		},
		{
			name: "four digit code",
			text: "校验码：4821",
			want: "4821",
		},
		{
			name: "eight digit code",
			text: "Your security code: 41926087",
			want: "41926087",
		},
		{
			name: "uppercase alphanumeric code",
			text: "验证码 A3F9K2 有效期 15 分钟",
			want: "A3F9K2",
		},
		{
			name: "six digits beat a closer four digit run",
			text: "验证码 12 分钟内有效：238877",
			want: "238877",
		},
		{
			name:    "subject wins over body",
			subject: "验证码：135790",
			text:    "另一个验证码 246802 已失效",
			want:    "135790",
		},
		{
			name: "script contents are ignored",
			html: "<div>验证码</div><script>var otp = 999999;</script><div>314159</div>",
			want: "314159",
		},
		{
			name: "html entities are decoded",
			html: "<p>验证码&nbsp;&nbsp;620913</p>",
			want: "620913",
		},
		{
			name: "keyword is case insensitive",
			text: "YOUR VERIFICATION CODE IS 707070",
			want: "707070",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			code, ok := DetectOTP(testCase.subject, testCase.text, testCase.html)
			if !ok || code != testCase.want {
				t.Fatalf("DetectOTP() = %q, %v; want %q, true", code, ok, testCase.want)
			}
		})
	}
}

func TestDetectOTPRejects(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		text    string
		html    string
	}{
		{
			name:    "no keyword means no code",
			subject: "订单 583920 已发货",
			text:    "您的订单 583920 已经发货，快递单号 SF1234567890。",
		},
		{
			name: "promo code is not an otp",
			text: "使用优惠码 SAVE20 立减 20 元，活动截止 2026 年 8 月 20 日。",
		},
		{
			name: "coupon code keyword is not matched",
			text: "Use discount code 445566 at checkout for 10% off.",
			// "discount code" is not in the keyword list; a bare "code" must not fire.
		},
		{
			name: "keyword inside a longer word does not fire",
			text: "Shipping notice 778899 for your parcel.",
		},
		{
			name: "year next to keyword is rejected",
			text: "验证码功能自 2019 年起启用",
		},
		{
			name: "date component is rejected",
			text: "验证码 2026-08-20 之后停止发送",
		},
		{
			name: "amount is rejected",
			text: "验证码服务费 ¥1200 已扣除",
		},
		{
			name: "digit run that is too long is rejected",
			text: "验证码发送至 13800138000",
		},
		{
			name: "lowercase mixed token is rejected",
			text: "验证码 step2 已完成",
		},
		{
			name: "keyword with no nearby digits",
			text: "验证码已过期，请重新获取新的凭据以继续登录当前账户，谢谢配合，如有疑问请联系客服 support",
		},
		{
			name: "empty input",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if code, ok := DetectOTP(testCase.subject, testCase.text, testCase.html); ok {
				t.Fatalf("DetectOTP() = %q, true; want no detection", code)
			}
		})
	}
}

func TestHTMLToTextSeparatesBlocks(t *testing.T) {
	got := htmlToText("<table><tr><td>123456</td><td>10分钟</td></tr></table>")
	for _, unwanted := range []string{"12345610"} {
		if contains(got, unwanted) {
			t.Fatalf("htmlToText() = %q, must not contain %q", got, unwanted)
		}
	}
	if !contains(got, "123456") || !contains(got, "10分钟") {
		t.Fatalf("htmlToText() = %q, want both cell values preserved", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for offset := 0; offset+len(needle) <= len(haystack); offset++ {
		if haystack[offset:offset+len(needle)] == needle {
			return offset
		}
	}
	return -1
}
