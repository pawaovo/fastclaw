package v1

import (
	"context"

	"github.com/fastclaw-ai/fastclaw/middleware"
	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/fastclaw-ai/fastclaw/service/runtime"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
)

// ProviderRequest represents a request to add/update a provider
type ProviderRequest struct {
	Name    string                      `json:"name"`
	BaseURL string                      `json:"baseUrl,omitempty"`
	APIKey  string                      `json:"apiKey,omitempty"`
	Auth    string                      `json:"auth,omitempty"`
	API     string                      `json:"api,omitempty"`
	Models  []model.ProviderModelConfig `json:"models,omitempty"`
}

// ListModelProviders returns all model providers for a bot
// GET /bots/:id/config/models
func ListModelProviders(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	config, err := bot.GetOpenClawConfig()
	if err != nil {
		return util.InternalError(c, "failed to get bot config")
	}

	// Return providers map
	providers := make(map[string]*model.ProviderConfig)
	if config.Models != nil && config.Models.Providers != nil {
		providers = config.Models.Providers
	}

	return util.Success(c, providers)
}

// AddModelProvider adds a new model provider to the bot
// POST /bots/:id/config/models
func AddModelProvider(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	var req ProviderRequest
	if err := c.Bind(&req); err != nil {
		return util.BadRequest(c, "invalid request body")
	}

	if req.Name == "" {
		return util.BadRequest(c, "provider name is required")
	}

	config, err := bot.GetOpenClawConfig()
	if err != nil {
		config = &model.OpenClawConfig{}
	}

	// Ensure models section exists
	if config.Models == nil {
		config.Models = &model.ModelsConfig{
			Mode:      "merge",
			Providers: make(map[string]*model.ProviderConfig),
		}
	}
	if config.Models.Providers == nil {
		config.Models.Providers = make(map[string]*model.ProviderConfig)
	}

	// Check if provider already exists
	if _, exists := config.Models.Providers[req.Name]; exists {
		return util.BadRequest(c, "provider already exists")
	}

	// Add provider
	config.Models.Providers[req.Name] = &model.ProviderConfig{
		BaseURL: req.BaseURL,
		APIKey:  req.APIKey,
		Auth:    req.Auth,
		API:     req.API,
		Models:  req.Models,
	}

	// Save to database
	if err := bot.SetOpenClawConfig(config); err != nil {
		return util.InternalError(c, "failed to set config")
	}
	if err := model.UpdateBot(bot); err != nil {
		return util.InternalError(c, "failed to update bot")
	}

	// Sync only models section to pod if bot is running (don't touch gateway)
	if bot.Status == model.BotStatusRunning && !runtime.IsDockerPoolMode() {
		go func() {
			ctx := context.Background()
			if err := k8s.SyncSectionsToPod(ctx, bot.ID, "models"); err != nil {
				c.Logger().Errorf("failed to sync config to pod: %v", err)
			}
		}()
	}

	return util.Success(c, config.Models.Providers[req.Name])
}

// GetModelProvider returns a single model provider by name
// GET /bots/:id/config/models/:provider
func GetModelProvider(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	providerName := c.Param("provider")
	if providerName == "" {
		return util.BadRequest(c, "provider name is required")
	}

	config, err := bot.GetOpenClawConfig()
	if err != nil {
		return util.InternalError(c, "failed to get bot config")
	}

	if config.Models == nil || config.Models.Providers == nil {
		return util.NotFound(c, "provider not found")
	}

	provider, exists := config.Models.Providers[providerName]
	if !exists {
		return util.NotFound(c, "provider not found")
	}

	return util.Success(c, provider)
}

// UpdateModelProvider updates a model provider configuration
// PUT /bots/:id/config/models/:provider
func UpdateModelProvider(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	providerName := c.Param("provider")
	if providerName == "" {
		return util.BadRequest(c, "provider name is required")
	}

	var req ProviderRequest
	if err := c.Bind(&req); err != nil {
		return util.BadRequest(c, "invalid request body")
	}

	config, err := bot.GetOpenClawConfig()
	if err != nil {
		config = &model.OpenClawConfig{}
	}

	// Ensure models section exists
	if config.Models == nil {
		config.Models = &model.ModelsConfig{
			Mode:      "merge",
			Providers: make(map[string]*model.ProviderConfig),
		}
	}
	if config.Models.Providers == nil {
		config.Models.Providers = make(map[string]*model.ProviderConfig)
	}

	// Update provider
	config.Models.Providers[providerName] = &model.ProviderConfig{
		BaseURL: req.BaseURL,
		APIKey:  req.APIKey,
		Auth:    req.Auth,
		API:     req.API,
		Models:  req.Models,
	}

	// Save to database
	if err := bot.SetOpenClawConfig(config); err != nil {
		return util.InternalError(c, "failed to set config")
	}
	if err := model.UpdateBot(bot); err != nil {
		return util.InternalError(c, "failed to update bot")
	}

	// Sync only models section to pod if bot is running (don't touch gateway)
	if bot.Status == model.BotStatusRunning && !runtime.IsDockerPoolMode() {
		go func() {
			ctx := context.Background()
			if err := k8s.SyncSectionsToPod(ctx, bot.ID, "models"); err != nil {
				c.Logger().Errorf("failed to sync config to pod: %v", err)
			}
		}()
	}

	return util.Success(c, config.Models.Providers[providerName])
}

// DeleteModelProvider removes a model provider
// DELETE /bots/:id/config/models/:provider
func DeleteModelProvider(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	providerName := c.Param("provider")
	if providerName == "" {
		return util.BadRequest(c, "provider name is required")
	}

	config, err := bot.GetOpenClawConfig()
	if err != nil {
		return util.InternalError(c, "failed to get bot config")
	}

	if config.Models == nil || config.Models.Providers == nil {
		return util.NotFound(c, "provider not found")
	}

	if _, exists := config.Models.Providers[providerName]; !exists {
		return util.NotFound(c, "provider not found")
	}

	// Delete provider
	delete(config.Models.Providers, providerName)

	// Save to database
	if err := bot.SetOpenClawConfig(config); err != nil {
		return util.InternalError(c, "failed to set config")
	}
	if err := model.UpdateBot(bot); err != nil {
		return util.InternalError(c, "failed to update bot")
	}

	// Sync only models section to pod if bot is running (don't touch gateway)
	if bot.Status == model.BotStatusRunning && !runtime.IsDockerPoolMode() {
		go func() {
			ctx := context.Background()
			if err := k8s.SyncSectionsToPod(ctx, bot.ID, "models"); err != nil {
				c.Logger().Errorf("failed to sync config to pod: %v", err)
			}
		}()
	}

	return util.Success(c, nil)
}
