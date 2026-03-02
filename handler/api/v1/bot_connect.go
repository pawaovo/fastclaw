package v1

import (
	"context"
	"strings"

	"github.com/fastclaw-ai/fastclaw/middleware"
	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/fastclaw-ai/fastclaw/service/runtime"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
	"github.com/spf13/viper"
)

type BotConnectResponse struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Status     model.BotStatus `json:"status"`
	Ready      bool            `json:"ready"`
	Token      string          `json:"token,omitempty"`
	Endpoint   string          `json:"endpoint,omitempty"`
	WsURL      string          `json:"ws_url,omitempty"`
	WebChatURL string          `json:"webchat_url,omitempty"`
}

func GetBotConnect(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	response := BotConnectResponse{
		ID:     bot.ID,
		Name:   bot.Name,
		Status: bot.Status,
		Token:  bot.ID, // Token is bot ID
	}

	if bot.Status != model.BotStatusRunning {
		return util.Success(c, response)
	}

	ctx := context.Background()

	ready, err := runtime.GetBotReady(ctx, bot)
	if err != nil {
		return util.InternalError(c, "failed to get bot status")
	}
	response.Ready = ready

	// Get runtime endpoint (internal)
	endpoint, err := runtime.GetBotEndpoint(ctx, bot)
	if err != nil {
		return util.InternalError(c, "failed to get bot endpoint")
	}
	response.Endpoint = endpoint

	// Build external URL based on domain template
	if ready {
		serviceName := ""
		namespace := ""
		if !runtime.IsDockerPoolMode() {
			serviceName = k8s.GetServiceName(bot.ID)
			namespace = k8s.GetNamespace()
		}

		// Get domain template: app-level first, then config file fallback
		var domainTemplate string
		if app, err := model.GetAppByID(bot.AppID); err == nil && app.BotDomainTemplate != "" {
			domainTemplate = app.BotDomainTemplate
		} else {
			domainTemplate = viper.GetString("domain.bot_domain_template")
		}

		// Replace placeholders
		externalURL := strings.ReplaceAll(domainTemplate, "{bot_id}", bot.ID)
		externalURL = strings.ReplaceAll(externalURL, "{service_name}", serviceName)
		externalURL = strings.ReplaceAll(externalURL, "{namespace}", namespace)

		response.WebChatURL = externalURL
		response.WsURL = strings.Replace(externalURL, "http://", "ws://", 1)
		response.WsURL = strings.Replace(response.WsURL, "https://", "wss://", 1)
	}

	return util.Success(c, response)
}
