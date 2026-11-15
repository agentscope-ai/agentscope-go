package app

import (
	"fmt"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/credential"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/embedding"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"
	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/tts"
)

// GetChatModelForCredential maps a credential's provider to the correct model
// constructor and returns a configured ChatModel for the given model name.
func GetChatModelForCredential(cred credential.Credential, modelName string) (model.ChatModel, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is nil")
	}
	if modelName == "" {
		return nil, fmt.Errorf("model name is required")
	}

	switch cred.Provider() {
	case "openai":
		return model.NewOpenAIChatModel(model.OpenAIConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   modelName,
		})
	case "anthropic":
		return model.NewAnthropicChatModel(&model.AnthropicConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   modelName,
		})
	case "dashscope":
		return model.NewDashScopeChatModel(model.DashScopeConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   modelName,
		})
	case "deepseek":
		return model.NewDeepSeekChatModel(model.DeepSeekConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   modelName,
		})
	case "gemini":
		return model.NewGeminiChatModel(model.GeminiConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   modelName,
		})
	case "moonshot":
		return model.NewMoonshotChatModel(model.MoonshotConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   modelName,
		})
	case "ollama":
		return model.NewOllamaChatModel(model.OllamaConfig{
			BaseURL: cred.BaseURL(),
			Model:   modelName,
		})
	case "xai":
		return model.NewXAIChatModel(model.XAIConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   modelName,
		})
	case "kimi":
		// Kimi uses the Moonshot API with a different default base URL.
		return model.NewMoonshotChatModel(model.MoonshotConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   modelName,
		})
	default:
		return nil, fmt.Errorf("chat model provider %q not supported", cred.Provider())
	}
}

// EmbeddingModelConfig describes which embedding model to create.
type EmbeddingModelConfig struct {
	Provider     string `json:"provider"`
	CredentialID string `json:"credential_id"`
	Model        string `json:"model"`
}

// GetEmbeddingModel resolves a credential and constructs an embedding model.
func GetEmbeddingModel(config EmbeddingModelConfig, registry *credential.Registry) (embedding.EmbeddingModel, error) {
	cred, err := registry.Get(config.Provider)
	if err != nil {
		return nil, fmt.Errorf("credential %q not found: %w", config.Provider, err)
	}

	switch config.Provider {
	case "openai":
		return embedding.NewOpenAIEmbeddingModel(&embedding.OpenAICompatConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   config.Model,
		})
	case "dashscope":
		if embedding.IsMultimodalModel(config.Model) {
			return embedding.NewDashScopeMultimodalEmbeddingModel(embedding.DashScopeMultimodalConfig{
				APIKey: cred.APIKey(),
				Model:  config.Model,
			})
		}
		return embedding.NewDashScopeEmbeddingModel(&embedding.OpenAICompatConfig{
			APIKey:  cred.APIKey(),
			BaseURL: cred.BaseURL(),
			Model:   config.Model,
		})
	case "ollama":
		return embedding.NewOllamaEmbeddingModel(&embedding.OpenAICompatConfig{
			BaseURL: cred.BaseURL(),
			Model:   config.Model,
		})
	case "gemini":
		return embedding.NewGeminiEmbeddingModel(&embedding.GeminiConfig{
			APIKey: cred.APIKey(),
			Model:  config.Model,
		})
	default:
		return nil, fmt.Errorf("embedding provider %q not supported", config.Provider)
	}
}

// TTSModelConfig describes which TTS model to create.
type TTSModelConfig struct {
	Provider     string `json:"provider"`
	CredentialID string `json:"credential_id"`
	Model        string `json:"model"`
}

// GetTTSModel resolves a credential and constructs a TTS model.
func GetTTSModel(config TTSModelConfig, registry *credential.Registry) (tts.Model, error) {
	cred, err := registry.Get(config.Provider)
	if err != nil {
		return nil, fmt.Errorf("credential %q not found: %w", config.Provider, err)
	}

	switch config.Provider {
	case "dashscope":
		return tts.NewDashScopeTTSModel(tts.DashScopeConfig{
			APIKey: cred.APIKey(),
			Model:  config.Model,
		})
	default:
		return nil, fmt.Errorf("TTS provider %q not supported", config.Provider)
	}
}
