package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

var ErrModelQuotaExhausted = errors.New("模型 API 额度或余额不可用，请恢复额度后重新保存模型配置")

const quotaStopReason = "api_quota_exhausted"
const quotaStopMessage = "模型 API 额度或余额已耗尽，监控已结束；恢复额度后请重新保存模型配置并新建监控"

// Provider failures are classified only from API error metadata, never OCR/ASR
// words. HTTP 429 alone is a temporary rate limit, not exhausted account quota.
func modelProviderError(status int, code, kind, message string) error {
	code = strings.ToLower(strings.TrimSpace(code))
	kind = strings.ToLower(strings.TrimSpace(kind))
	exhausted := status == http.StatusPaymentRequired
	switch code {
	case "insufficient_quota", "credit_balance_exhausted", "insufficient_balance",
		"organization_spend_limit_exceeded", "project_spend_limit_exceeded",
		"organization_usage_limit_exceeded", "1008", "2056":
		exhausted = true
	}
	if kind == "insufficient_quota" || kind == "insufficient_balance" {
		exhausted = true
	}
	text := strings.ToLower(message)
	if containsAny(text, []string{
		"insufficient balance", "insufficient credits", "balance is not enough",
		"credit balance exhausted", "exceeded your current quota", "quota exhausted",
		"余额不足", "余额已耗尽", "额度已耗尽",
	}) {
		exhausted = true
	}
	if exhausted {
		return fmt.Errorf("%w (HTTP %d, code=%s, type=%s)", ErrModelQuotaExhausted, status, code, kind)
	}
	return fmt.Errorf("model provider returned HTTP %d (code=%s, type=%s)", status, code, kind)
}

func providerErrorCode(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}
