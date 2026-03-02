package portal

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/fastclaw-ai/fastclaw/service/runtime"
	"github.com/labstack/echo/v4"
	"github.com/spf13/viper"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"gorm.io/gorm"
)

const (
	sessionCookieName = "fastclaw_portal_session"
	stateCookieName   = "fastclaw_portal_state"
)

type sessionClaims struct {
	UserID string `json:"uid"`
	Email  string `json:"email"`
	Exp    int64  `json:"exp"`
}

type portalBotResponse struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Slug      string          `json:"slug"`
	Status    model.BotStatus `json:"status"`
	AccessURL string          `json:"access_url"`
	Endpoint  string          `json:"endpoint"`
}

type portalAIConfigRequest struct {
	Provider      string `json:"provider"`
	BaseURL       string `json:"baseUrl"`
	APIKey        string `json:"apiKey"`
	APIType       string `json:"apiType"`
	Auth          string `json:"auth"`
	ModelID       string `json:"modelId"`
	ModelName     string `json:"modelName"`
	MaxTokens     int    `json:"maxTokens"`
	ContextWindow int    `json:"contextWindow"`
}

type portalAIConfigResponse struct {
	Provider      string `json:"provider"`
	BaseURL       string `json:"baseUrl,omitempty"`
	APIType       string `json:"apiType,omitempty"`
	Auth          string `json:"auth,omitempty"`
	ModelID       string `json:"modelId,omitempty"`
	ModelName     string `json:"modelName,omitempty"`
	MaxTokens     int    `json:"maxTokens,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	HasAPIKey     bool   `json:"hasApiKey"`
}

var (
	portalAppOnce sync.Once
	portalAppID   string
	portalAppErr  error
)

func RegisterRoutes(e *echo.Echo) {
	e.GET("/portal", portalPage)
	e.GET("/portal/auth/google/login", googleLogin)
	e.GET("/portal/auth/google/callback", googleCallback)
	e.POST("/portal/auth/local/register", localRegister)
	e.POST("/portal/auth/local/login", localLogin)
	e.POST("/portal/auth/logout", logout)

	api := e.Group("/portal/api")
	api.GET("/me", me)
	api.GET("/bots", listBots)
	api.POST("/bots", createBot)
	api.POST("/bots/:id/start", startBot)
	api.POST("/bots/:id/stop", stopBot)
	api.GET("/bots/:id/ai-config", getBotAIConfig)
	api.PUT("/bots/:id/ai-config", updateBotAIConfig)
	api.POST("/allocate", allocateBot)
}

func portalCanonicalBaseURL() string {
	redirectURL := strings.TrimSpace(viper.GetString("portal.google_redirect_url"))
	if redirectURL == "" {
		return ""
	}
	u, err := url.Parse(redirectURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func portalPage(c echo.Context) error {
	loginURL := "/portal/auth/google/login"
	if base := portalCanonicalBaseURL(); base != "" {
		loginURL = strings.TrimRight(base, "/") + "/portal/auth/google/login"
	}
	page := portalHTML
	page = strings.Replace(page, "__GOOGLE_LOGIN_URL__", html.EscapeString(loginURL), 1)
	if isGoogleAuthEnabled() {
		page = strings.Replace(page, "__GOOGLE_HIDDEN_CLASS__", "", 1)
	} else {
		page = strings.Replace(page, "__GOOGLE_HIDDEN_CLASS__", "hidden", 1)
	}
	if isLocalAuthEnabled() {
		page = strings.Replace(page, "__LOCAL_HIDDEN_CLASS__", "", 1)
	} else {
		page = strings.Replace(page, "__LOCAL_HIDDEN_CLASS__", "hidden", 1)
	}
	return c.HTML(http.StatusOK, page)
}

func googleLogin(c echo.Context) error {
	if !isGoogleAuthEnabled() {
		return c.String(http.StatusBadRequest, "google oauth is disabled")
	}
	cfg, err := getGoogleConfig()
	if err != nil {
		return c.String(http.StatusBadRequest, err.Error())
	}

	state := randomHex(16)
	c.SetCookie(&http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   false,
		MaxAge:   600,
		SameSite: http.SameSiteLaxMode,
	})

	return c.Redirect(http.StatusFound, cfg.AuthCodeURL(state))
}

func googleCallback(c echo.Context) error {
	if !isGoogleAuthEnabled() {
		return c.String(http.StatusBadRequest, "google oauth is disabled")
	}
	cfg, err := getGoogleConfig()
	if err != nil {
		return c.String(http.StatusBadRequest, err.Error())
	}

	state := c.QueryParam("state")
	code := c.QueryParam("code")
	if state == "" || code == "" {
		return c.String(http.StatusBadRequest, "missing oauth parameters")
	}
	stateCookie, err := c.Cookie(stateCookieName)
	if err != nil || stateCookie == nil || stateCookie.Value != state {
		return c.String(http.StatusBadRequest, "invalid oauth state")
	}

	token, err := cfg.Exchange(c.Request().Context(), code)
	if err != nil {
		return c.String(http.StatusBadRequest, "failed to exchange code")
	}

	userInfo, err := fetchGoogleUserInfo(c.Request().Context(), cfg, token)
	if err != nil {
		return c.String(http.StatusBadRequest, "failed to fetch google userinfo")
	}

	user, err := model.UpsertGooglePortalUser(userInfo.Email, userInfo.Name, userInfo.Picture, userInfo.Sub)
	if err != nil {
		return c.String(http.StatusInternalServerError, "failed to save user")
	}

	if err := setSessionCookie(c, user); err != nil {
		return c.String(http.StatusInternalServerError, "failed to create session")
	}

	// Login success -> allocate a dedicated bot so user can directly use it.
	_, _ = ensureUserBotRunning(user)

	c.SetCookie(&http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		SameSite: http.SameSiteLaxMode,
	})
	return c.Redirect(http.StatusFound, "/portal")
}

type localAuthRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

func localRegister(c echo.Context) error {
	if !isLocalAuthEnabled() {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "local auth is disabled"})
	}

	var req localAuthRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "invalid request"})
	}

	user, err := model.CreateLocalPortalUser(req.Email, req.Name, req.Password)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
	}

	if err := setSessionCookie(c, user); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to create session"})
	}
	_, _ = ensureUserBotRunning(user)
	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

func localLogin(c echo.Context) error {
	if !isLocalAuthEnabled() {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "local auth is disabled"})
	}

	var req localAuthRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "invalid request"})
	}

	user, err := model.AuthenticateLocalPortalUser(req.Email, req.Password)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
	}

	if err := setSessionCookie(c, user); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to create session"})
	}
	_, _ = ensureUserBotRunning(user)
	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

func logout(c echo.Context) error {
	c.SetCookie(&http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		SameSite: http.SameSiteLaxMode,
	})
	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

func me(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}

	bots, err := listUserBots(user)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to list bots"})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"ok": true,
		"user": map[string]any{
			"id":         user.ID,
			"email":      user.Email,
			"name":       user.Name,
			"avatar_url": user.AvatarURL,
		},
		"bots": bots,
	})
}

func listBots(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bots, err := listUserBots(user)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to list bots"})
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "bots": bots})
}

func allocateBot(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := ensureUserBotRunning(user)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "bot": toPortalBot(bot)})
}

type createBotRequest struct {
	Name string `json:"name"`
}

func createBot(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}

	var req createBotRequest
	_ = c.Bind(&req)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = fmt.Sprintf("%s's Bot", strings.TrimSpace(user.Name))
	}

	appID, err := getPortalAppID()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to get portal app"})
	}

	bot := &model.Bot{
		AppID:  appID,
		UserID: user.ID,
		Name:   name,
		Status: model.BotStatusCreated,
	}
	if err := model.CreateBot(bot); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to create bot"})
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "bot": toPortalBot(bot)})
}

func startBot(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := mustOwnBot(c.Param("id"), user)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]any{"ok": false, "message": err.Error()})
	}
	if bot.Status != model.BotStatusRunning {
		endpoint, err := runtime.StartBot(context.Background(), bot, nil)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to start bot: " + err.Error()})
		}
		if err := model.UpdateBotStatus(bot.ID, model.BotStatusRunning, endpoint); err != nil {
			_ = runtime.ReleaseBot(bot.ID)
			return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to update bot status"})
		}
		bot.Status = model.BotStatusRunning
		bot.Endpoint = endpoint
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "bot": toPortalBot(bot)})
}

func stopBot(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := mustOwnBot(c.Param("id"), user)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]any{"ok": false, "message": err.Error()})
	}

	if bot.Status == model.BotStatusRunning {
		if err := runtime.StopBot(context.Background(), bot); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to stop bot"})
		}
		if err := model.UpdateBotStatus(bot.ID, model.BotStatusStopped, ""); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to update bot status"})
		}
		bot.Status = model.BotStatusStopped
		bot.Endpoint = ""
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "bot": toPortalBot(bot)})
}

func getBotAIConfig(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := mustOwnBot(c.Param("id"), user)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]any{"ok": false, "message": err.Error()})
	}

	resp, err := buildPortalAIConfigResponse(bot, strings.TrimSpace(c.QueryParam("provider")))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to read ai config"})
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "config": resp})
}

func updateBotAIConfig(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := mustOwnBot(c.Param("id"), user)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]any{"ok": false, "message": err.Error()})
	}

	var req portalAIConfigRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "invalid request body"})
	}

	req.Provider = strings.TrimSpace(req.Provider)
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	req.APIKey = strings.TrimSpace(req.APIKey)
	req.APIType = strings.TrimSpace(req.APIType)
	req.Auth = strings.TrimSpace(req.Auth)
	req.ModelID = strings.TrimSpace(req.ModelID)
	req.ModelName = strings.TrimSpace(req.ModelName)

	if req.Provider == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "provider is required"})
	}
	if req.ModelID == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "modelId is required"})
	}
	if req.APIType == "" {
		req.APIType = "openai-completions"
	}
	if req.Auth == "" {
		req.Auth = "api-key"
	}

	config, err := bot.GetOpenClawConfig()
	if err != nil {
		config = &model.OpenClawConfig{}
	}
	if config.Models == nil {
		config.Models = &model.ModelsConfig{
			Mode:      "merge",
			Providers: make(map[string]*model.ProviderConfig),
		}
	}
	if config.Models.Providers == nil {
		config.Models.Providers = make(map[string]*model.ProviderConfig)
	}

	provider, ok := config.Models.Providers[req.Provider]
	if !ok || provider == nil {
		provider = &model.ProviderConfig{}
	}

	provider.BaseURL = req.BaseURL
	provider.API = req.APIType
	provider.Auth = req.Auth
	if req.APIKey != "" {
		provider.APIKey = req.APIKey
	}

	modelName := req.ModelName
	if modelName == "" {
		modelName = req.ModelID
	}
	modelCfg := model.ProviderModelConfig{
		ID:   req.ModelID,
		Name: modelName,
	}
	if req.ContextWindow > 0 {
		modelCfg.ContextWindow = req.ContextWindow
	}
	if req.MaxTokens > 0 {
		modelCfg.MaxTokens = req.MaxTokens
	}
	provider.Models = []model.ProviderModelConfig{modelCfg}
	config.Models.Providers[req.Provider] = provider

	if config.Agents == nil {
		config.Agents = &model.AgentsConfig{}
	}
	if config.Agents.Defaults == nil {
		config.Agents.Defaults = &model.AgentDefaultsConfig{}
	}
	if config.Agents.Defaults.Model == nil {
		config.Agents.Defaults.Model = &model.AgentModelConfig{}
	}
	config.Agents.Defaults.Model.Primary = req.Provider + "/" + req.ModelID

	if err := bot.SetOpenClawConfig(config); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to persist ai config"})
	}
	if err := model.UpdateBot(bot); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to save bot"})
	}

	if bot.Status == model.BotStatusRunning {
		if runtime.IsDockerPoolMode() {
			if err := runtime.SyncBotConfigSections(context.Background(), bot, "models", "agents"); err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to apply config to running bot: " + err.Error()})
			}
		} else {
			if err := k8s.SyncSectionsToPod(context.Background(), bot.ID, "models", "agents"); err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to sync config to bot"})
			}
		}
	}

	resp, err := buildPortalAIConfigResponse(bot, req.Provider)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to read saved ai config"})
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "config": resp})
}

func buildPortalAIConfigResponse(bot *model.Bot, requestedProvider string) (*portalAIConfigResponse, error) {
	config, err := bot.GetOpenClawConfig()
	if err != nil {
		return nil, err
	}

	resp := &portalAIConfigResponse{
		Provider: "custom",
		APIType:  "openai-completions",
		Auth:     "api-key",
	}

	if config == nil || config.Models == nil || len(config.Models.Providers) == 0 {
		return resp, nil
	}

	providerName := requestedProvider
	if providerName == "" && config.Agents != nil && config.Agents.Defaults != nil && config.Agents.Defaults.Model != nil {
		primary := strings.TrimSpace(config.Agents.Defaults.Model.Primary)
		if idx := strings.Index(primary, "/"); idx > 0 {
			providerName = strings.TrimSpace(primary[:idx])
		}
	}
	if providerName == "" {
		for k := range config.Models.Providers {
			providerName = k
			break
		}
	}
	if providerName == "" {
		return resp, nil
	}

	p := config.Models.Providers[providerName]
	if p == nil {
		return resp, nil
	}

	resp.Provider = providerName
	resp.BaseURL = p.BaseURL
	if p.API != "" {
		resp.APIType = p.API
	}
	if p.Auth != "" {
		resp.Auth = p.Auth
	}
	resp.HasAPIKey = strings.TrimSpace(p.APIKey) != ""
	if len(p.Models) > 0 {
		resp.ModelID = p.Models[0].ID
		resp.ModelName = p.Models[0].Name
		resp.MaxTokens = p.Models[0].MaxTokens
		resp.ContextWindow = p.Models[0].ContextWindow
	}
	return resp, nil
}

func listUserBots(user *model.PortalUser) ([]portalBotResponse, error) {
	appID, err := getPortalAppID()
	if err != nil {
		return nil, err
	}
	bots, err := model.ListBotsByAppAndUser(appID, user.ID)
	if err != nil {
		return nil, err
	}
	out := make([]portalBotResponse, 0, len(bots))
	for _, b := range bots {
		out = append(out, toPortalBot(b))
	}
	return out, nil
}

func ensureUserBotRunning(user *model.PortalUser) (*model.Bot, error) {
	appID, err := getPortalAppID()
	if err != nil {
		return nil, err
	}
	bots, err := model.ListBotsByAppAndUser(appID, user.ID)
	if err != nil {
		return nil, err
	}

	var bot *model.Bot
	if len(bots) == 0 {
		name := strings.TrimSpace(user.Name)
		if name == "" {
			name = "My Bot"
		}
		bot = &model.Bot{
			AppID:  appID,
			UserID: user.ID,
			Name:   name,
			Status: model.BotStatusCreated,
		}
		if err := model.CreateBot(bot); err != nil {
			return nil, err
		}
	} else {
		bot = bots[0]
	}

	if bot.Status != model.BotStatusRunning {
		endpoint, err := runtime.StartBot(context.Background(), bot, nil)
		if err != nil {
			return nil, err
		}
		if err := model.UpdateBotStatus(bot.ID, model.BotStatusRunning, endpoint); err != nil {
			_ = runtime.ReleaseBot(bot.ID)
			return nil, err
		}
		bot.Status = model.BotStatusRunning
		bot.Endpoint = endpoint
	}
	return bot, nil
}

func mustOwnBot(botID string, user *model.PortalUser) (*model.Bot, error) {
	bot, err := model.GetBotByID(botID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, errors.New("bot not found")
		}
		return nil, err
	}
	appID, err := getPortalAppID()
	if err != nil {
		return nil, err
	}
	if bot.UserID != user.ID || bot.AppID != appID {
		return nil, errors.New("forbidden")
	}
	return bot, nil
}

func getPortalAppID() (string, error) {
	portalAppOnce.Do(func() {
		if configuredID := strings.TrimSpace(viper.GetString("portal.app_id")); configuredID != "" {
			if _, err := model.GetAppByID(configuredID); err == nil {
				portalAppID = configuredID
				return
			}
		}

		appName := strings.TrimSpace(viper.GetString("portal.app_name"))
		if appName == "" {
			appName = "FastClaw Portal"
		}

		app, err := model.GetAppByName(appName)
		if err == nil {
			portalAppID = app.ID
			return
		}
		if err != gorm.ErrRecordNotFound {
			portalAppErr = err
			return
		}

		newApp := &model.App{
			Name:   appName,
			Status: "active",
		}
		if err := model.CreateApp(newApp); err != nil {
			portalAppErr = err
			return
		}
		portalAppID = newApp.ID
	})
	return portalAppID, portalAppErr
}

func toPortalBot(bot *model.Bot) portalBotResponse {
	return portalBotResponse{
		ID:        bot.ID,
		Name:      bot.Name,
		Slug:      bot.Slug,
		Status:    bot.Status,
		Endpoint:  bot.Endpoint,
		AccessURL: buildAccessURL(bot),
	}
}

func buildAccessURL(bot *model.Bot) string {
	if runtime.IsDockerPoolMode() {
		base := strings.TrimSpace(viper.GetString("domain.api_domain"))
		if base == "" {
			port := viper.GetInt("server.port")
			if port == 0 {
				port = 18080
			}
			base = fmt.Sprintf("http://127.0.0.1:%d", port)
		}
		base = strings.TrimRight(base, "/")
		return fmt.Sprintf("%s/proxy/%s/", base, bot.Slug)
	}
	domain := strings.TrimSpace(viper.GetString("domain.bot_domain_suffix"))
	if domain == "" {
		domain = "fastclaw.ai"
	}
	return fmt.Sprintf("https://%s.%s?token=%s", bot.Slug, domain, bot.AccessToken)
}

func isGoogleAuthEnabled() bool {
	_, err := getGoogleConfig()
	return err == nil
}

func isLocalAuthEnabled() bool {
	// Default enabled for no-domain deployments unless explicitly disabled.
	if !viper.IsSet("portal.local_auth_enabled") {
		return true
	}
	return viper.GetBool("portal.local_auth_enabled")
}

func getGoogleConfig() (*oauth2.Config, error) {
	clientID := strings.TrimSpace(viper.GetString("portal.google_client_id"))
	clientSecret := strings.TrimSpace(viper.GetString("portal.google_client_secret"))
	redirectURL := strings.TrimSpace(viper.GetString("portal.google_redirect_url"))
	if clientID == "" || clientSecret == "" || redirectURL == "" {
		return nil, errors.New("portal google oauth is not configured")
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     google.Endpoint,
		Scopes:       []string{"openid", "email", "profile"},
	}, nil
}

type googleUserInfo struct {
	Sub     string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

func fetchGoogleUserInfo(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token) (*googleUserInfo, error) {
	client := cfg.Client(ctx, token)
	resp, err := client.Get("https://www.googleapis.com/oauth2/v3/userinfo")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google userinfo status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var info googleUserInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, err
	}
	if strings.TrimSpace(info.Email) == "" {
		return nil, errors.New("google email is empty")
	}
	return &info, nil
}

func mustSessionUser(c echo.Context) (*model.PortalUser, error) {
	cookie, err := c.Cookie(sessionCookieName)
	if err != nil || cookie == nil || cookie.Value == "" {
		return nil, errors.New("missing session")
	}
	claims, err := parseSession(cookie.Value)
	if err != nil {
		return nil, err
	}
	if claims.Exp < time.Now().Unix() {
		return nil, errors.New("session expired")
	}
	return model.GetPortalUserByID(claims.UserID)
}

func setSessionCookie(c echo.Context, user *model.PortalUser) error {
	exp := time.Now().Add(7 * 24 * time.Hour).Unix()
	claims := sessionClaims{
		UserID: user.ID,
		Email:  user.Email,
		Exp:    exp,
	}
	token, err := signSession(claims)
	if err != nil {
		return err
	}
	c.SetCookie(&http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   false,
		MaxAge:   7 * 24 * 3600,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func signSession(claims sessionClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	payloadEncoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(getSessionSecret()))
	_, _ = mac.Write([]byte(payloadEncoded))
	sig := hex.EncodeToString(mac.Sum(nil))
	return payloadEncoded + "." + sig, nil
}

func parseSession(token string) (*sessionClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, errors.New("invalid session token")
	}
	payloadEncoded, sig := parts[0], parts[1]

	mac := hmac.New(sha256.New, []byte(getSessionSecret()))
	_, _ = mac.Write([]byte(payloadEncoded))
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return nil, errors.New("invalid session signature")
	}

	payload, err := base64.RawURLEncoding.DecodeString(payloadEncoded)
	if err != nil {
		return nil, err
	}

	var claims sessionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	return &claims, nil
}

func getSessionSecret() string {
	secret := strings.TrimSpace(viper.GetString("portal.session_secret"))
	if secret != "" {
		return secret
	}
	// Fallback for local/testing.
	admin := strings.TrimSpace(viper.GetString("api.admin_token"))
	if admin != "" {
		return admin
	}
	return "fastclaw-portal-dev-secret"
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const portalHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>FastClaw Portal</title>
  <style>
    :root {
      --bg: #f4f6f8;
      --card: #ffffff;
      --text: #1f2937;
      --muted: #6b7280;
      --line: #e5e7eb;
      --primary: #2563eb;
      --primary-hover: #1d4ed8;
      --danger: #dc2626;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
      background: radial-gradient(circle at top left, #eef2ff 0%, var(--bg) 45%, #f8fafc 100%);
      color: var(--text);
    }
    .wrap {
      max-width: 920px;
      margin: 48px auto;
      padding: 0 16px;
    }
    .card {
      background: var(--card);
      border: 1px solid var(--line);
      border-radius: 16px;
      padding: 24px;
      box-shadow: 0 12px 30px rgba(15, 23, 42, 0.06);
      margin-bottom: 16px;
    }
    h1 {
      margin: 0 0 8px;
      font-size: 28px;
      font-weight: 700;
      letter-spacing: 0.2px;
    }
    .sub {
      margin: 0;
      color: var(--muted);
      font-size: 14px;
    }
    .row {
      display: flex;
      gap: 10px;
      flex-wrap: wrap;
      align-items: center;
      margin-top: 16px;
    }
    button {
      border: 1px solid var(--line);
      border-radius: 10px;
      padding: 10px 14px;
      background: #fff;
      color: var(--text);
      cursor: pointer;
      font-size: 14px;
    }
    button.primary {
      background: var(--primary);
      color: #fff;
      border-color: var(--primary);
    }
    button.primary:hover { background: var(--primary-hover); }
    button.danger {
      border-color: #fecaca;
      color: var(--danger);
      background: #fff5f5;
    }
    input {
      border: 1px solid var(--line);
      border-radius: 10px;
      padding: 10px 12px;
      font-size: 14px;
      min-width: 200px;
    }
    .field {
      display: flex;
      flex-direction: column;
      gap: 6px;
      min-width: 220px;
    }
    .field label {
      color: var(--muted);
      font-size: 12px;
      font-weight: 600;
      letter-spacing: 0.2px;
    }
    .field input {
      min-width: 0;
      width: 100%;
    }
    .ai-panel {
      margin-top: 12px;
      border-top: 1px dashed var(--line);
      padding-top: 12px;
    }
    .bots {
      display: grid;
      grid-template-columns: 1fr;
      gap: 12px;
      margin-top: 12px;
    }
    .bot {
      border: 1px solid var(--line);
      border-radius: 12px;
      padding: 14px;
      background: #fff;
    }
    .bot h3 {
      margin: 0 0 6px;
      font-size: 16px;
    }
    .meta {
      color: var(--muted);
      font-size: 13px;
      margin: 4px 0;
      word-break: break-all;
    }
    .hidden { display: none; }
  </style>
</head>
<body>
  <div class="wrap">
    <div class="card">
      <h1>FastClaw Portal</h1>
      <p class="sub">支持邮箱注册/登录（无域名可用）与 Google 登录（可选）。登录后自动分配专属 OpenClaw，并支持一个用户管理多个 Bot。</p>
      <div id="authArea" class="row hidden">
        <div class="row __LOCAL_HIDDEN_CLASS__" style="width:100%">
          <input id="loginEmail" placeholder="邮箱" />
          <input id="loginPassword" type="password" placeholder="密码（至少8位）" />
          <input id="registerName" placeholder="昵称（注册可选）" />
          <button class="primary" onclick="loginLocal()">邮箱登录</button>
          <button onclick="registerLocal()">邮箱注册</button>
        </div>
        <div class="row __GOOGLE_HIDDEN_CLASS__" style="width:100%">
          <button class="primary" onclick="window.location='__GOOGLE_LOGIN_URL__'">使用 Google 登录</button>
        </div>
        <div id="authMsg" class="meta" style="width:100%"></div>
      </div>
      <div id="userArea" class="hidden">
        <div class="row" id="userInfo"></div>
        <div class="row">
          <button class="primary" onclick="allocateBot()">自动分配并启动专属 Bot</button>
          <button onclick="refresh()">刷新</button>
          <button class="danger" onclick="logout()">退出登录</button>
        </div>
      </div>
    </div>

    <div id="manageCard" class="card hidden">
      <h2 style="margin-top:0">我的 Bot</h2>
      <div class="row">
        <input id="botName" placeholder="新 Bot 名称（可选）" />
        <button class="primary" onclick="createBot()">创建 Bot</button>
      </div>
      <div id="bots" class="bots"></div>
    </div>
  </div>

  <script>
    let currentUser = null;
    let currentBots = [];

    async function api(url, options = {}) {
      const res = await fetch(url, {
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        ...options,
      });
      return res.json();
    }

    function setAuthMessage(msg, isError = false) {
      const el = document.getElementById('authMsg');
      if (!el) return;
      el.textContent = msg || '';
      el.style.color = isError ? '#dc2626' : '#6b7280';
    }

    function render() {
      const authArea = document.getElementById('authArea');
      const userArea = document.getElementById('userArea');
      const manageCard = document.getElementById('manageCard');
      const userInfo = document.getElementById('userInfo');
      const botsEl = document.getElementById('bots');

      if (!currentUser) {
        authArea.classList.remove('hidden');
        userArea.classList.add('hidden');
        manageCard.classList.add('hidden');
        return;
      }

      authArea.classList.add('hidden');
      userArea.classList.remove('hidden');
      manageCard.classList.remove('hidden');
      userInfo.innerHTML = '<strong>' + (currentUser.name || 'User') + '</strong><span style="color:#6b7280">' + currentUser.email + '</span>';

      botsEl.innerHTML = '';
      for (const b of currentBots) {
        const div = document.createElement('div');
        div.className = 'bot';
        const safeId = b.id.replace(/[^a-zA-Z0-9_-]/g, '');
        div.innerHTML =
          '<h3>' + b.name + '</h3>' +
          '<div class="meta">Status: ' + b.status + ' | Slug: ' + b.slug + '</div>' +
          '<div class="meta">Endpoint: ' + (b.endpoint || '-') + '</div>' +
          '<div class="row">' +
            '<a href="' + b.access_url + '" target="_blank"><button class="primary">Open</button></a>' +
            '<button onclick="startBot(\'' + b.id + '\')">Start</button>' +
            '<button onclick="stopBot(\'' + b.id + '\')">Stop</button>' +
            '<button onclick="toggleAIConfig(\'' + safeId + '\', \'' + b.id + '\')">AI Config</button>' +
          '</div>' +
          '<div class="meta">' + b.access_url + '</div>' +
          '<div id="aiWrap-' + safeId + '" class="ai-panel hidden">' +
            '<div class="row">' +
              '<div class="field"><label>Provider</label><input id="ai-provider-' + safeId + '" placeholder="custom / openai / google" /></div>' +
              '<div class="field"><label>Base URL</label><input id="ai-baseurl-' + safeId + '" placeholder="https://api.openai.com/v1" /></div>' +
              '<div class="field"><label>API Key</label><input id="ai-apikey-' + safeId + '" type="password" placeholder="sk-..." /></div>' +
            '</div>' +
            '<div class="row">' +
              '<div class="field"><label>Model ID</label><input id="ai-modelid-' + safeId + '" placeholder="gpt-4o-mini" /></div>' +
              '<div class="field"><label>Model Name</label><input id="ai-modelname-' + safeId + '" placeholder="gpt-4o-mini" /></div>' +
              '<div class="field"><label>API Type</label><input id="ai-apitype-' + safeId + '" placeholder="openai-completions" /></div>' +
            '</div>' +
            '<div class="row">' +
              '<div class="field"><label>Auth</label><input id="ai-auth-' + safeId + '" placeholder="api-key / bearer" /></div>' +
              '<div class="field"><label>Max Tokens</label><input id="ai-max-' + safeId + '" type="number" min="0" placeholder="4096" /></div>' +
              '<div class="field"><label>Context Window</label><input id="ai-context-' + safeId + '" type="number" min="0" placeholder="128000" /></div>' +
            '</div>' +
            '<div class="row">' +
              '<button class="primary" onclick="saveAIConfig(\'' + safeId + '\', \'' + b.id + '\')">Save AI Config</button>' +
              '<span id="ai-status-' + safeId + '" class="meta"></span>' +
            '</div>' +
          '</div>';
        botsEl.appendChild(div);
      }
    }

    async function refresh() {
      try {
        const data = await api('/portal/api/me');
        if (!data.ok) {
          currentUser = null;
          currentBots = [];
        } else {
          currentUser = data.user;
          currentBots = data.bots || [];
        }
      } catch (e) {
        currentUser = null;
        currentBots = [];
      }
      render();
    }

    async function loginLocal() {
      const email = document.getElementById('loginEmail').value.trim();
      const password = document.getElementById('loginPassword').value;
      setAuthMessage('');
      const data = await api('/portal/auth/local/login', {
        method: 'POST',
        body: JSON.stringify({ email, password }),
      });
      if (!data.ok) {
        setAuthMessage(data.message || '登录失败', true);
        return;
      }
      setAuthMessage('登录成功');
      await refresh();
    }

    async function registerLocal() {
      const email = document.getElementById('loginEmail').value.trim();
      const password = document.getElementById('loginPassword').value;
      const name = document.getElementById('registerName').value.trim();
      setAuthMessage('');
      const data = await api('/portal/auth/local/register', {
        method: 'POST',
        body: JSON.stringify({ email, password, name }),
      });
      if (!data.ok) {
        setAuthMessage(data.message || '注册失败', true);
        return;
      }
      setAuthMessage('注册成功');
      await refresh();
    }

    async function allocateBot() {
      await api('/portal/api/allocate', { method: 'POST' });
      await refresh();
    }

    async function createBot() {
      const name = document.getElementById('botName').value.trim();
      await api('/portal/api/bots', { method: 'POST', body: JSON.stringify({ name }) });
      document.getElementById('botName').value = '';
      await refresh();
    }

    async function startBot(id) {
      await api('/portal/api/bots/' + id + '/start', { method: 'POST' });
      await refresh();
    }

    async function stopBot(id) {
      await api('/portal/api/bots/' + id + '/stop', { method: 'POST' });
      await refresh();
    }

    async function toggleAIConfig(safeId, botId) {
      const wrap = document.getElementById('aiWrap-' + safeId);
      if (!wrap) return;
      if (!wrap.classList.contains('hidden')) {
        wrap.classList.add('hidden');
        return;
      }
      wrap.classList.remove('hidden');
      await loadAIConfig(safeId, botId);
    }

    async function loadAIConfig(safeId, botId) {
      const data = await api('/portal/api/bots/' + botId + '/ai-config');
      if (!data.ok || !data.config) {
        setAIStatus(safeId, 'Load failed');
        return;
      }
      const cfg = data.config;
      setInput('ai-provider-' + safeId, cfg.provider || 'custom');
      setInput('ai-baseurl-' + safeId, cfg.baseUrl || '');
      setInput('ai-apikey-' + safeId, '');
      setInput('ai-modelid-' + safeId, cfg.modelId || '');
      setInput('ai-modelname-' + safeId, cfg.modelName || '');
      setInput('ai-apitype-' + safeId, cfg.apiType || 'openai-completions');
      setInput('ai-auth-' + safeId, cfg.auth || 'api-key');
      setInput('ai-max-' + safeId, cfg.maxTokens || '');
      setInput('ai-context-' + safeId, cfg.contextWindow || '');
      setAIStatus(safeId, cfg.hasApiKey ? 'API Key already set' : 'API Key not set');
    }

    async function saveAIConfig(safeId, botId) {
      const payload = {
        provider: getInput('ai-provider-' + safeId),
        baseUrl: getInput('ai-baseurl-' + safeId),
        apiKey: getInput('ai-apikey-' + safeId),
        modelId: getInput('ai-modelid-' + safeId),
        modelName: getInput('ai-modelname-' + safeId),
        apiType: getInput('ai-apitype-' + safeId),
        auth: getInput('ai-auth-' + safeId),
        maxTokens: parseInt(getInput('ai-max-' + safeId), 10) || 0,
        contextWindow: parseInt(getInput('ai-context-' + safeId), 10) || 0
      };
      const data = await api('/portal/api/bots/' + botId + '/ai-config', {
        method: 'PUT',
        body: JSON.stringify(payload)
      });
      if (!data.ok) {
        setAIStatus(safeId, 'Save failed: ' + (data.message || 'unknown error'));
        return;
      }
      setInput('ai-apikey-' + safeId, '');
      setAIStatus(safeId, 'Saved');
      await refresh();
    }

    function getInput(id) {
      const el = document.getElementById(id);
      if (!el) return '';
      return (el.value || '').trim();
    }

    function setInput(id, value) {
      const el = document.getElementById(id);
      if (!el) return;
      el.value = value == null ? '' : String(value);
    }

    function setAIStatus(safeId, msg) {
      const el = document.getElementById('ai-status-' + safeId);
      if (!el) return;
      el.textContent = msg;
    }

    async function logout() {
      await api('/portal/auth/logout', { method: 'POST' });
      await refresh();
    }

    refresh();
  </script>
</body>
</html>`
