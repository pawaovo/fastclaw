package v1

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/middleware"
	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/runtime"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
	"github.com/spf13/viper"
)

type CreateBotRequest struct {
	UserID    string                 `json:"user_id" validate:"required"`
	Name      string                 `json:"name" validate:"required"`
	Slug      string                 `json:"slug,omitempty"` // Optional custom slug
	Config    map[string]interface{} `json:"config,omitempty"`
	ExpiresAt *time.Time             `json:"expires_at,omitempty"` // Optional expiration time
}

type BotResponse struct {
	*model.Bot
	AccessURL string `json:"access_url"`
}

func CreateBot(c echo.Context) error {
	var req CreateBotRequest
	if err := c.Bind(&req); err != nil {
		return util.BadRequest(c, "invalid request body")
	}

	if req.UserID == "" {
		return util.BadRequest(c, "user_id is required")
	}
	if req.Name == "" {
		return util.BadRequest(c, "name is required")
	}

	// Validate slug if provided
	if req.Slug != "" {
		if !isValidSlug(req.Slug) {
			return util.BadRequest(c, "slug must be 1-50 characters, lowercase letters, numbers, and hyphens only")
		}
		// Check if slug is already taken
		existing, _ := model.GetBotBySlug(req.Slug)
		if existing != nil {
			return util.BadRequest(c, "slug is already taken")
		}
	}

	// Get app_id from authenticated app context
	var appID string
	if app := middleware.GetAppFromContext(c); app != nil {
		appID = app.ID
	}

	hasExisting, err := model.HasBotForAppAndUser(appID, req.UserID)
	if err != nil {
		return util.InternalError(c, "failed to check user bot quota")
	}
	if hasExisting {
		return util.BadRequest(c, model.ErrUserBotLimitExceeded.Error())
	}

	bot := &model.Bot{
		AppID:     appID,
		UserID:    req.UserID,
		Name:      req.Name,
		Slug:      req.Slug, // Will be auto-generated if empty
		Status:    model.BotStatusCreated,
		ExpiresAt: req.ExpiresAt,
	}

	// Set config if provided (OpenClaw native format)
	if req.Config != nil {
		if err := bot.SetConfigMap(req.Config); err != nil {
			return util.InternalError(c, "failed to set config")
		}
	}

	if err := model.CreateBot(bot); err != nil {
		return util.InternalError(c, "failed to create bot")
	}

	return util.Success(c, &BotResponse{
		Bot:       bot,
		AccessURL: buildAccessURL(bot.Slug, bot.AccessToken),
	})
}

func isValidSlug(slug string) bool {
	if len(slug) < 1 || len(slug) > 50 {
		return false
	}
	// Allow single character or multiple characters (must start and end with alphanumeric)
	matched, _ := regexp.MatchString("^[a-z0-9]([a-z0-9-]*[a-z0-9])?$", slug)
	return matched
}

func buildAccessURL(slug, token string) string {
	if runtime.IsDockerPoolMode() {
		apiDomain := strings.TrimSpace(viper.GetString("domain.api_domain"))
		port := viper.GetInt("server.port")
		if port == 0 {
			port = 18080
		}

		var url string
		if strings.HasPrefix(apiDomain, "http://") || strings.HasPrefix(apiDomain, "https://") {
			url = strings.TrimRight(apiDomain, "/") + "/proxy/" + slug + "/"
		} else {
			if apiDomain == "" {
				apiDomain = "127.0.0.1"
			}
			url = fmt.Sprintf("http://%s:%d/proxy/%s/", apiDomain, port, slug)
		}
		return url
	}

	domain := viper.GetString("domain.bot_domain_suffix")
	if domain == "" {
		domain = "fastclaw.ai"
	}
	url := fmt.Sprintf("https://%s.%s", slug, domain)
	if token != "" {
		url += "?token=" + token
	}
	return url
}
