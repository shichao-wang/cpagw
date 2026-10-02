package config

import (
	"strings"
	"testing"
)

func TestValidateKey(t *testing.T) {
	if err := ValidateKey("sk-local.test_123"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", " ", "secret\nsecond", "secret\tother", "secret\x7f"} {
		err := ValidateKey(key)
		if err == nil {
			t.Fatal("无效 key 未被拒绝")
		}
		if strings.TrimSpace(key) != "" && strings.Contains(err.Error(), key) {
			t.Fatal("错误消息不应含凭证")
		}
	}
}
