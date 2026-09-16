package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"liveguard/internal/domain"
)

type Planner interface {
	Plan(context.Context, string, string) ([]domain.CheckSpec, string, error)
}

type ResilientPlanner struct {
	OpenAI        *OpenAIPlanner
	Deterministic DeterministicPlanner
}

func (p ResilientPlanner) Plan(ctx context.Context, targetURL, objective string) ([]domain.CheckSpec, string, error) {
	if p.OpenAI != nil {
		checks, source, err := p.OpenAI.Plan(ctx, targetURL, objective)
		if err == nil {
			return checks, source, nil
		}
		checks, _, fallbackErr := p.Deterministic.Plan(ctx, targetURL, objective)
		if fallbackErr != nil {
			return nil, "", errors.Join(err, fallbackErr)
		}
		return checks, "deterministic_fallback", nil
	}
	return p.Deterministic.Plan(ctx, targetURL, objective)
}

type DeterministicPlanner struct{}

func (DeterministicPlanner) Plan(_ context.Context, _ string, objective string) ([]domain.CheckSpec, string, error) {
	terms := meaningfulTerms(objective)
	checks := []domain.CheckSpec{
		{Key: "reachable", Label: "页面可以访问", Kind: "page_reachable"},
		{Key: "title", Label: "页面标题可以识别", Kind: "title_present"},
		{Key: "challenge", Label: "没有登录或安全验证阻塞", Kind: "challenge_absent"},
		{Key: "objective_terms", Label: "页面包含与巡检目标相关的信息", Kind: "contains_any", Terms: terms},
		{Key: "live_status", Label: "直播状态可以识别", Kind: "live_status"},
	}
	if strings.Contains(objective, "活动入口") {
		checks = append(checks, domain.CheckSpec{Key: "activity_entry", Label: "活动入口已经开启", Kind: "activity_entry"})
	}
	return checks, "deterministic", nil
}

type OpenAIPlanner struct {
	APIKey  string
	Model   string
	Client  *http.Client
	Breaker *CircuitBreaker
}

type responseEnvelope struct {
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

func (p *OpenAIPlanner) Plan(ctx context.Context, targetURL, objective string) ([]domain.CheckSpec, string, error) {
	if err := p.Breaker.BeforeRequest(); err != nil {
		return nil, "", err
	}
	prompt := `You plan read-only QA checks for a livestream webpage. Return JSON only as {"checks":[...]}. ` +
		`Use 2-6 checks. Allowed kinds: page_reachable, title_present, challenge_absent, contains_any, live_status, activity_entry. ` +
		`Each check requires key, label, kind; contains_any also requires 1-5 short visible-text terms. ` +
		`Never request login bypass, posting, following, liking, gifting, payment, or arbitrary code execution.` +
		"\nURL: " + targetURL + "\nObjective: " + objective
	body := map[string]any{
		"model":             p.Model,
		"instructions":      "You produce small, auditable browser inspection plans.",
		"input":             prompt,
		"max_output_tokens": 1000,
		"store":             false,
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/responses", bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.Client.Do(req)
	if err != nil {
		p.Breaker.Failure()
		return nil, "", fmt.Errorf("model request: %w", err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, 2<<20)
	if resp.StatusCode/100 != 2 {
		p.Breaker.Failure()
		payload, _ := io.ReadAll(io.LimitReader(limited, 2048))
		return nil, "", fmt.Errorf("model returned %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	p.Breaker.Success()
	var envelope responseEnvelope
	if err := json.NewDecoder(limited).Decode(&envelope); err != nil {
		return nil, "", err
	}
	var output string
	for _, item := range envelope.Output {
		for _, content := range item.Content {
			if content.Type == "output_text" {
				output += content.Text
			}
		}
	}
	output = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(output), "```json"), "```"))
	var parsed struct {
		Checks []domain.CheckSpec `json:"checks"`
	}
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		return nil, "", fmt.Errorf("invalid plan JSON: %w", err)
	}
	if err := validateChecks(parsed.Checks); err != nil {
		return nil, "", err
	}
	return parsed.Checks, "openai:" + p.Model, nil
}

func validateChecks(checks []domain.CheckSpec) error {
	if len(checks) < 2 || len(checks) > 6 {
		return errors.New("plan must contain 2-6 checks")
	}
	allowed := map[string]bool{"page_reachable": true, "title_present": true, "challenge_absent": true, "contains_any": true, "live_status": true, "activity_entry": true}
	seen := map[string]bool{}
	for i := range checks {
		check := &checks[i]
		check.Key = strings.TrimSpace(check.Key)
		check.Label = strings.TrimSpace(check.Label)
		if check.Key == "" || check.Label == "" || !allowed[check.Kind] || seen[check.Key] {
			return errors.New("plan contains an invalid or duplicate check")
		}
		seen[check.Key] = true
		if check.Kind == "contains_any" && (len(check.Terms) == 0 || len(check.Terms) > 5) {
			return errors.New("contains_any requires 1-5 terms")
		}
	}
	return nil
}

func meaningfulTerms(input string) []string {
	candidates := []string{"主播昵称", "主播", "直播状态", "直播", "互动入口", "互动", "活动入口", "活动", "登录", "安全验证", "验证码", "页面标题"}
	terms := make([]string, 0, 4)
	for _, candidate := range candidates {
		if strings.Contains(input, candidate) {
			terms = append(terms, candidate)
			if len(terms) == 4 {
				return terms
			}
		}
	}
	replacer := strings.NewReplacer("，", " ", "。", " ", ",", " ", ".", " ", "：", " ", ":", " ")
	parts := strings.Fields(replacer.Replace(input))
	for _, part := range parts {
		if len([]rune(part)) < 2 {
			continue
		}
		duplicate := false
		for _, existing := range terms {
			if existing == part {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		terms = append(terms, part)
		if len(terms) == 4 {
			break
		}
	}
	if len(terms) == 0 {
		terms = []string{"直播", "主播"}
	}
	return terms
}

func DefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 25 * time.Second}
}
