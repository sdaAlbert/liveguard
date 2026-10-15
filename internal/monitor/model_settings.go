package monitor

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"liveguard/internal/agent"
)

type ModelSettingsInput struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Endpoint       string `json:"endpoint"`
	APIKey         string `json:"api_key,omitempty"`
	SupportsVision *bool  `json:"supports_vision,omitempty"`
}
type ModelSettingsStatus struct {
	QuotaExhausted  bool                           `json:"quota_exhausted"`
	Provider        string                         `json:"provider"`
	Model           string                         `json:"model"`
	Endpoint        string                         `json:"endpoint,omitempty"`
	Configured      bool                           `json:"configured"`
	KeyPresent      bool                           `json:"key_present"`
	Capabilities    ModelCapabilities              `json:"capabilities"`
	ProviderOptions map[string]ModelProviderOption `json:"provider_options"`
}

// ModelProviderOption is the server-owned default shown when a provider is
// selected in the UI. Runtime configuration and UI defaults share this source.
type ModelProviderOption struct {
	Model        string            `json:"model"`
	Models       []string          `json:"models"`
	VisionModels []string          `json:"vision_models"`
	Endpoint     string            `json:"endpoint"`
	Capabilities ModelCapabilities `json:"capabilities"`
}

func modelProviderOptions() map[string]ModelProviderOption {
	options := make(map[string]ModelProviderOption, len(providerDefaults)+1)
	for provider, defaults := range providerDefaults {
		visionModels := make([]string, 0, len(defaults.Models))
		for _, model := range defaults.Models {
			if defaultVisionCapability(provider, model) {
				visionModels = append(visionModels, model)
			}
		}
		options[provider] = ModelProviderOption{
			Model: defaults.Model, Endpoint: defaults.Endpoint,
			Models: append([]string(nil), defaults.Models...), VisionModels: visionModels,
			Capabilities: ModelCapabilities{
				ToolCalling: true,
				Vision:      defaultVisionCapability(provider, defaults.Model),
			},
		}
	}
	options["rules"] = ModelProviderOption{}
	return options
}

func (s *Service) ModelSettings() ModelSettingsStatus {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()
	status := s.modelConfig
	status.ProviderOptions = modelProviderOptions()
	if state, ok := s.reviewer.(interface{ QuotaExhausted() bool }); ok {
		status.QuotaExhausted = state.QuotaExhausted()
	}
	return status
}

// ConfigureModel changes a resolved model binding. All providers use the same
// planner/reviewer; config selects only transport, credentials and capabilities.
func (s *Service) ConfigureModel(input ModelSettingsInput) (ModelSettingsStatus, error) {
	provider := strings.ToLower(strings.TrimSpace(input.Provider))
	requestedModel := strings.TrimSpace(input.Model)
	if requestedModel == "" {
		requestedModel = providerDefaults[provider].Model
	}
	if strings.TrimSpace(input.APIKey) == "" {
		s.modelMu.RLock()
		if s.modelConfig.Provider == provider && s.modelConfig.KeyPresent {
			input.APIKey = s.modelAPIKey
			if input.SupportsVision == nil && requestedModel == s.modelConfig.Model {
				vision := s.modelConfig.Capabilities.Vision
				input.SupportsVision = &vision
			}
		}
		s.modelMu.RUnlock()
	}
	config, err := resolveModelConfig(input)
	if err != nil {
		return ModelSettingsStatus{}, err
	}
	var planner GoalPlanner = ruleGoalPlanner{}
	var reviewer UnexpectedEventReviewer
	var baseline BaselineAnalyzer
	if config.Provider != "rules" {
		model := configuredToolModel(config)
		planner = &AgentGoalPlanner{Model: model}
		reviewer = &AgentReviewer{Model: model, Breaker: agent.NewCircuitBreaker(2, 30*time.Second)}
		if config.Capabilities.Vision {
			baseline = &AgentBaselineAnalyzer{Model: model}
		}
	}
	status := ModelSettingsStatus{
		Provider: config.Provider, Model: config.Model, Endpoint: config.Endpoint,
		Configured: reviewer != nil, KeyPresent: reviewer != nil, Capabilities: config.Capabilities,
		ProviderOptions: modelProviderOptions(),
	}
	s.modelMu.Lock()
	s.goalPlanner, s.reviewer, s.modelAPIKey, s.modelConfig = planner, reviewer, config.APIKey, status
	s.baselineAnalyzer = baseline
	s.modelMu.Unlock()
	s.onUpdate()
	return status, nil
}

func resolveModelConfig(input ModelSettingsInput) (ModelConfig, error) {
	provider := strings.ToLower(strings.TrimSpace(input.Provider))
	if provider == "" || provider == "rules" {
		return ModelConfig{Provider: "rules"}, nil
	}
	defaults, ok := providerDefaults[provider]
	if !ok {
		return ModelConfig{}, errors.New("目前只支持 MiniMax、OpenAI、DeepSeek 或本地规则模式")
	}
	config := ModelConfig{
		Provider: provider, Model: strings.TrimSpace(input.Model),
		Endpoint: strings.TrimSpace(input.Endpoint), APIKey: strings.TrimSpace(input.APIKey),
		Capabilities: ModelCapabilities{ToolCalling: true},
	}
	if config.Model == "" {
		config.Model = defaults.Model
	}
	if config.Endpoint == "" {
		config.Endpoint = defaults.Endpoint
	}
	if config.APIKey == "" {
		return ModelConfig{}, errors.New("请填写 API Key")
	}
	if err := validateModelEndpoint(config.Endpoint); err != nil {
		return ModelConfig{}, err
	}
	config.Capabilities.Vision = defaultVisionCapability(provider, config.Model)
	if input.SupportsVision != nil {
		config.Capabilities.Vision = *input.SupportsVision
	}
	return config, nil
}

func validateModelEndpoint(endpoint string) error {
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || parsed.Hostname() == "" ||
		(parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1"))) {
		return fmt.Errorf("API 地址必须使用 HTTPS；本机调试可使用 localhost HTTP")
	}
	return nil
}
