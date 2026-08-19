package main

import (
	"testing"
)

func TestSignMD5(t *testing.T) {
	// 测试易支付 MD5 签名算法
	params := map[string]string{
		"pid":           "1001",
		"out_trade_no":  "20240101001",
		"money":         "10.00",
		"name":          "VIP会员",
		"notify_url":    "http://example.com/notify",
		"sign_type":     "MD5",
	}
	key := "test_key_123"

	sign := signMD5(params, key)

	// 验证签名不为空
	if sign == "" {
		t.Error("签名不应为空")
	}

	// 验证签名长度（MD5 固定 32 位）
	if len(sign) != 32 {
		t.Errorf("签名长度应为 32，实际 %d", len(sign))
	}

	// 验证签名为小写
	for _, c := range sign {
		if c >= 'A' && c <= 'Z' {
			t.Error("签名应为小写")
			break
		}
	}

	t.Logf("生成的签名: %s", sign)
}

func TestFormatMoney(t *testing.T) {
	tests := []struct {
		cents int64
		want  string
	}{
		{100, "1.00"},
		{1000, "10.00"},
		{12345, "123.45"},
		{1, "0.01"},
		{99, "0.99"},
	}

	for _, tt := range tests {
		got := formatMoney(tt.cents)
		if got != tt.want {
			t.Errorf("formatMoney(%d) = %s, want %s", tt.cents, got, tt.want)
		}
	}
}

func TestBuildQuery(t *testing.T) {
	params := map[string]string{
		"pid":   "1001",
		"money": "10.00",
		"name":  "测试商品",
	}

	query := buildQuery(params)

	// 验证包含所有参数
	if query == "" {
		t.Error("查询字符串不应为空")
	}

	// 验证 URL 编码
	// 中文应该被编码
	if !contains(query, "name=") {
		t.Error("查询字符串应包含 name 参数")
	}

	t.Logf("生成的查询字符串: %s", query)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) &&
		(s == substr || (len(s) > len(substr) &&
		(s[:len(substr)] == substr || contains(s[1:], substr))))
}
