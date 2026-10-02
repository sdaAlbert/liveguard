package monitor

import (
	"context"
	"strings"
)

// SignalAnalyzer runs a server-owned evidence processor. It never receives
// model-generated code, login profiles, filesystem paths or API credentials.
type SignalAnalyzer interface {
	Analyze(context.Context, []SignalEvent) (SignalAnalysis, error)
}

type SignalAnalysis struct {
	Lines             int `json:"lines"`
	UniqueLines       int `json:"unique_lines"`
	NegationLines     int `json:"negation_lines"`
	VerificationLines int `json:"verification_lines"`
}

// LocalSignalAnalyzer computes bounded text statistics directly in Go.
// Counts are observations, never semantic proof that an event happened.
type LocalSignalAnalyzer struct{}

func (LocalSignalAnalyzer) Analyze(ctx context.Context, events []SignalEvent) (SignalAnalysis, error) {
	var result SignalAnalysis
	var snippets []string
	for _, event := range events[:min(len(events), 6)] {
		snippets = append(snippets, event.Text)
	}
	text := []rune(strings.Join(snippets, "\n"))
	if len(text) > 1100 {
		text = text[:1100]
	}
	seen := make(map[string]bool)
	for _, raw := range strings.Split(string(text), "\n") {
		if err := ctx.Err(); err != nil {
			return SignalAnalysis{}, err
		}
		line := strings.Join(strings.Fields(raw), " ")
		if line == "" {
			continue
		}
		result.Lines++
		if !seen[line] {
			result.UniqueLines++
			seen[line] = true
		}
		for _, word := range []string{"没有", "不会", "取消", "暂时不", "not happening", "cancelled"} {
			if strings.Contains(line, word) {
				result.NegationLines++
				break
			}
		}
		for _, word := range []string{"验证码", "安全验证", "请登录", "captcha"} {
			if strings.Contains(line, word) {
				result.VerificationLines++
				break
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return SignalAnalysis{}, err
	}
	return result, nil
}
