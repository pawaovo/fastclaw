package portal

import (
	"bytes"
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
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/fastclaw-ai/fastclaw/service/runtime"
	"github.com/fastclaw-ai/fastclaw/util"
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
	Health    string          `json:"health"`
	Ready     bool            `json:"ready"`
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
	APIKey        string `json:"apiKey,omitempty"`
	APIType       string `json:"apiType,omitempty"`
	Auth          string `json:"auth,omitempty"`
	ModelID       string `json:"modelId,omitempty"`
	ModelName     string `json:"modelName,omitempty"`
	MaxTokens     int    `json:"maxTokens,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	HasAPIKey     bool   `json:"hasApiKey"`
}

type portalChannelConfigRequest struct {
	Provider       string   `json:"provider"`
	Account        string   `json:"account"`
	BotToken       string   `json:"botToken"`
	Token          string   `json:"token"`
	AppID          string   `json:"appId"`
	AppSecret      string   `json:"appSecret"`
	DMPolicy       string   `json:"dmPolicy"`
	GroupPolicy    string   `json:"groupPolicy"`
	AllowFrom      []string `json:"allowFrom"`
	RequireMention *bool    `json:"requireMention"`
	Enabled        *bool    `json:"enabled"`
}

type portalChannelConfigResponse struct {
	Provider       string   `json:"provider"`
	Account        string   `json:"account,omitempty"`
	DMPolicy       string   `json:"dmPolicy,omitempty"`
	GroupPolicy    string   `json:"groupPolicy,omitempty"`
	AllowFrom      []string `json:"allowFrom,omitempty"`
	RequireMention bool     `json:"requireMention,omitempty"`
	Enabled        bool     `json:"enabled"`
	HasBotToken    bool     `json:"hasBotToken,omitempty"`
	HasToken       bool     `json:"hasToken,omitempty"`
	HasAppSecret   bool     `json:"hasAppSecret,omitempty"`
	AppID          string   `json:"appId,omitempty"`
}

type portalChannelPairingApproveRequest struct {
	Code string `json:"code"`
}

type portalPoolStatusResponse struct {
	Mode       string `json:"mode"`
	Total      int    `json:"total"`
	Capacity   int    `json:"capacity"`
	Active     int    `json:"active"`
	Free       int    `json:"free"`
	Full       bool   `json:"full"`
	MaxRunning int    `json:"maxRunning"`
}

var (
	portalAppOnce sync.Once
	portalAppID   string
	portalAppErr  error

	portalMustSessionUser      = mustSessionUser
	portalEnsureUserBotRunning = ensureUserBotRunning
	portalMustOwnBot           = mustOwnBot
	portalDeleteBotRecord      = model.DeleteBot

	portalRuntimeStartBot = runtime.StartBot
	portalRuntimeStopBot  = runtime.StopBot
	portalRuntimeRelease  = runtime.ReleaseBot
)

const poolExhaustedMessage = "资源池已满，请稍后重试（当前无可用 OpenClaw 实例）"

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
	api.GET("/pool", getPoolStatus)
	api.POST("/bots", createBot)
	api.POST("/bots/:id/start", startBot)
	api.POST("/bots/:id/stop", stopBot)
	api.DELETE("/bots/:id", deleteBot)
	api.GET("/bots/:id/health", getBotHealth)
	api.GET("/bots/:id/ai-config", getBotAIConfig)
	api.PUT("/bots/:id/ai-config", updateBotAIConfig)
	api.GET("/bots/:id/channels", getBotChannels)
	api.PUT("/bots/:id/channels/:channel", upsertBotChannel)
	api.DELETE("/bots/:id/channels/:channel", deleteBotChannel)
	api.POST("/bots/:id/channels/:channel/test", testBotChannel)
	api.POST("/bots/:id/channels/:channel/pairing/approve", approveBotChannelPairing)
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
	warn := ""
	if _, err := ensureUserBotRunning(user); err != nil {
		warn = humanizeAllocationError(err)
		log.Printf("portal google login: ensure bot failed for user=%s email=%s: %v", user.ID, user.Email, err)
	}

	c.SetCookie(&http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		SameSite: http.SameSiteLaxMode,
	})
	target := "/portal"
	if warn != "" {
		target += "?warn=" + url.QueryEscape(warn)
	}
	return c.Redirect(http.StatusFound, target)
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
	resp := map[string]any{"ok": true}
	if _, err := ensureUserBotRunning(user); err != nil {
		resp["warning"] = humanizeAllocationError(err)
		log.Printf("portal register: ensure bot failed for user=%s email=%s: %v", user.ID, user.Email, err)
	}
	return c.JSON(http.StatusOK, resp)
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
	resp := map[string]any{"ok": true}
	if _, err := ensureUserBotRunning(user); err != nil {
		resp["warning"] = humanizeAllocationError(err)
		log.Printf("portal login: ensure bot failed for user=%s email=%s: %v", user.ID, user.Email, err)
	}
	return c.JSON(http.StatusOK, resp)
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
	pool := buildPoolStatus()
	return c.JSON(http.StatusOK, map[string]any{
		"ok": true,
		"user": map[string]any{
			"id":         user.ID,
			"email":      user.Email,
			"name":       user.Name,
			"avatar_url": user.AvatarURL,
		},
		"bots": bots,
		"pool": pool,
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
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "bots": bots, "pool": buildPoolStatus()})
}

func getPoolStatus(c echo.Context) error {
	if _, err := mustSessionUser(c); err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "pool": buildPoolStatus()})
}

type createBotRequest struct {
	Name string `json:"name"`
}

func createBot(c echo.Context) error {
	user, err := portalMustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}

	// Portal policy: one OpenClaw instance per user.
	bot, err := portalEnsureUserBotRunning(user)
	if err != nil {
		if isPoolExhaustedError(err) {
			return poolExhaustedResponse(c)
		}
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to ensure dedicated bot: " + err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "bot": toPortalBot(bot)})
}

func startBot(c echo.Context) error {
	user, err := portalMustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := portalMustOwnBot(c.Param("id"), user)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]any{"ok": false, "message": err.Error()})
	}
	if bot.Status != model.BotStatusRunning {
		endpoint, err := portalRuntimeStartBot(context.Background(), bot, nil)
		if err != nil {
			if isPoolExhaustedError(err) {
				return poolExhaustedResponse(c)
			}
			return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to start bot: " + err.Error()})
		}
		if err := model.UpdateBotStatus(bot.ID, model.BotStatusRunning, endpoint); err != nil {
			_ = portalRuntimeRelease(bot.ID)
			return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to update bot status"})
		}
		bot.Status = model.BotStatusRunning
		bot.Endpoint = endpoint
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "bot": toPortalBot(bot)})
}

func isPoolExhaustedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "no free docker pool endpoint") ||
		strings.Contains(msg, "max running bots limit reached")
}

func humanizeAllocationError(err error) string {
	if isPoolExhaustedError(err) {
		return poolExhaustedMessage
	}
	return "实例分配失败，请点击“新增专属实例”重试"
}

func poolExhaustedResponse(c echo.Context) error {
	return c.JSON(http.StatusConflict, map[string]any{
		"ok":      false,
		"message": poolExhaustedMessage,
		"pool":    buildPoolStatus(),
	})
}

func buildPoolStatus() *portalPoolStatusResponse {
	mode := strings.TrimSpace(viper.GetString("runtime.mode"))
	maxRunning := viper.GetInt("runtime.max_running_bots")
	if maxRunning < 0 {
		maxRunning = 0
	}

	total := 0
	for _, ep := range viper.GetStringSlice("docker_pool.endpoints") {
		if strings.TrimSpace(ep) != "" {
			total++
		}
	}
	active := 0
	if util.GetDB() != nil {
		if c, err := model.CountActiveEndpointLeases(); err == nil {
			active = int(c)
		}
	}
	capacity := total
	if maxRunning > 0 && (capacity == 0 || maxRunning < capacity) {
		capacity = maxRunning
	}
	free := capacity - active
	if free < 0 {
		free = 0
	}

	return &portalPoolStatusResponse{
		Mode:       mode,
		Total:      total,
		Capacity:   capacity,
		Active:     active,
		Free:       free,
		Full:       capacity > 0 && active >= capacity,
		MaxRunning: maxRunning,
	}
}

func stopBot(c echo.Context) error {
	user, err := portalMustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := portalMustOwnBot(c.Param("id"), user)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]any{"ok": false, "message": err.Error()})
	}

	if bot.Status == model.BotStatusRunning {
		if err := portalRuntimeStopBot(context.Background(), bot); err != nil {
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

func deleteBot(c echo.Context) error {
	user, err := portalMustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := portalMustOwnBot(c.Param("id"), user)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]any{"ok": false, "message": err.Error()})
	}

	if bot.Status == model.BotStatusRunning {
		if err := portalRuntimeStopBot(context.Background(), bot); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to stop bot"})
		}
	}
	_ = portalRuntimeRelease(bot.ID)
	if err := portalDeleteBotRecord(bot.ID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to delete bot"})
	}

	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

func getBotHealth(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := mustOwnBot(c.Param("id"), user)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]any{"ok": false, "message": err.Error()})
	}

	health, ready := resolveBotHealth(bot)
	return c.JSON(http.StatusOK, map[string]any{
		"ok": true,
		"health": map[string]any{
			"status": health,
			"ready":  ready,
		},
	})
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

	cfgMap, err := bot.GetConfigMap()
	if err != nil || cfgMap == nil {
		cfgMap = map[string]interface{}{}
	}
	models := ensureMapField(cfgMap, "models")
	if stringFromMap(models, "mode") == "" {
		models["mode"] = "merge"
	}
	providers := ensureMapField(models, "providers")
	provider := ensureMapField(providers, req.Provider)
	provider["baseUrl"] = req.BaseURL
	provider["api"] = req.APIType
	provider["auth"] = req.Auth
	if req.APIKey != "" {
		provider["apiKey"] = req.APIKey
	}

	modelName := req.ModelName
	if modelName == "" {
		modelName = req.ModelID
	}
	modelCfg := map[string]interface{}{
		"id":   req.ModelID,
		"name": modelName,
	}
	if req.ContextWindow > 0 {
		modelCfg["contextWindow"] = req.ContextWindow
	}
	if req.MaxTokens > 0 {
		modelCfg["maxTokens"] = req.MaxTokens
	}
	provider["models"] = []map[string]interface{}{modelCfg}

	agents := ensureMapField(cfgMap, "agents")
	defaults := ensureMapField(agents, "defaults")
	modelNode := ensureMapField(defaults, "model")
	modelNode["primary"] = req.Provider + "/" + req.ModelID

	if err := bot.SetConfigMap(cfgMap); err != nil {
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
	cfgMap, err := bot.GetConfigMap()
	if err != nil {
		return nil, err
	}

	resp := &portalAIConfigResponse{
		Provider: "custom",
		APIType:  "openai-completions",
		Auth:     "api-key",
	}

	if cfgMap == nil {
		return resp, nil
	}
	models := toMap(cfgMap["models"])
	if models == nil {
		return resp, nil
	}
	providers := toMap(models["providers"])
	if len(providers) == 0 {
		return resp, nil
	}

	providerName := requestedProvider
	if providerName == "" {
		agents := toMap(cfgMap["agents"])
		defaults := toMap(agents["defaults"])
		modelNode := toMap(defaults["model"])
		primary := stringFromMap(modelNode, "primary")
		if idx := strings.Index(primary, "/"); idx > 0 {
			providerName = strings.TrimSpace(primary[:idx])
		}
	}
	if providerName != "" {
		if _, ok := providers[providerName]; !ok {
			providerName = ""
		}
	}
	if providerName == "" {
		if _, ok := providers["custom"]; ok {
			providerName = "custom"
		}
	}
	if providerName == "" {
		keys := make([]string, 0, len(providers))
		for k := range providers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			providerName = keys[0]
		}
	}
	if providerName == "" {
		return resp, nil
	}

	p := toMap(providers[providerName])
	if p == nil {
		return resp, nil
	}

	resp.Provider = providerName
	resp.BaseURL = stringFromMap(p, "baseUrl")
	if apiType := stringFromMap(p, "api"); apiType != "" {
		resp.APIType = apiType
	}
	if auth := stringFromMap(p, "auth"); auth != "" {
		resp.Auth = auth
	}
	resp.APIKey = stringFromMap(p, "apiKey")
	resp.HasAPIKey = resp.APIKey != ""
	if modelsRaw, ok := p["models"].([]interface{}); ok && len(modelsRaw) > 0 {
		m := toMap(modelsRaw[0])
		resp.ModelID = stringFromMap(m, "id")
		resp.ModelName = stringFromMap(m, "name")
		resp.MaxTokens = intFromMap(m, "maxTokens")
		resp.ContextWindow = intFromMap(m, "contextWindow")
	}
	if resp.ModelID == "" {
		resp.ModelID = stringFromMap(p, "modelId")
		if resp.ModelName == "" {
			resp.ModelName = stringFromMap(p, "modelName")
		}
		if resp.MaxTokens == 0 {
			resp.MaxTokens = intFromMap(p, "maxTokens")
		}
		if resp.ContextWindow == 0 {
			resp.ContextWindow = intFromMap(p, "contextWindow")
		}
	}
	return resp, nil
}

func getBotChannels(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := mustOwnBot(c.Param("id"), user)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]any{"ok": false, "message": err.Error()})
	}

	cfgMap, err := bot.GetConfigMap()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to read bot config"})
	}
	channels := map[string]interface{}{}
	if cfgMap != nil {
		if raw := toMap(cfgMap["channels"]); raw != nil {
			channels = raw
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"ok":        true,
		"channels":  channels,
		"summaries": buildChannelSummaries(channels),
	})
}

func upsertBotChannel(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := mustOwnBot(c.Param("id"), user)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]any{"ok": false, "message": err.Error()})
	}

	channel := strings.TrimSpace(strings.ToLower(c.Param("channel")))
	if channel == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "channel is required"})
	}

	var req portalChannelConfigRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "invalid request body"})
	}

	req.Provider = strings.TrimSpace(strings.ToLower(req.Provider))
	req.Account = strings.TrimSpace(req.Account)
	req.BotToken = strings.TrimSpace(req.BotToken)
	req.Token = strings.TrimSpace(req.Token)
	req.AppID = strings.TrimSpace(req.AppID)
	req.AppSecret = strings.TrimSpace(req.AppSecret)
	req.DMPolicy = strings.TrimSpace(strings.ToLower(req.DMPolicy))
	req.GroupPolicy = strings.TrimSpace(strings.ToLower(req.GroupPolicy))
	req.AllowFrom = normalizeStringList(req.AllowFrom)
	if req.Account == "" {
		req.Account = "default"
	}
	if req.Provider != "" && req.Provider != channel {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "provider does not match channel"})
	}
	if req.DMPolicy == "" {
		req.DMPolicy = "pairing"
	}
	if req.GroupPolicy == "" {
		req.GroupPolicy = "open"
	}

	cfgMap, err := bot.GetConfigMap()
	if err != nil || cfgMap == nil {
		cfgMap = map[string]interface{}{}
	}
	channels := ensureMapField(cfgMap, "channels")
	channelCfg := toMap(channels[channel])
	if channelCfg == nil {
		channelCfg = map[string]interface{}{}
	}
	channelCfg["enabled"] = true
	if req.Enabled != nil {
		channelCfg["enabled"] = *req.Enabled
	}
	channelCfg["dmPolicy"] = req.DMPolicy
	channelCfg["groupPolicy"] = req.GroupPolicy
	if len(req.AllowFrom) > 0 {
		channelCfg["allowFrom"] = req.AllowFrom
	} else {
		delete(channelCfg, "allowFrom")
	}
	if req.RequireMention != nil {
		channelCfg["requireMention"] = *req.RequireMention
	}

	switch channel {
	case "telegram":
		if req.BotToken != "" {
			channelCfg["botToken"] = req.BotToken
		}
	case "discord":
		if req.Token != "" {
			channelCfg["token"] = req.Token
		}
	case "feishu":
		accounts := toMap(channelCfg["accounts"])
		if accounts == nil {
			accounts = map[string]interface{}{}
		}
		accountCfg := toMap(accounts[req.Account])
		if accountCfg == nil {
			accountCfg = map[string]interface{}{}
		}
		if req.AppID != "" {
			accountCfg["appId"] = req.AppID
		}
		if req.AppSecret != "" {
			accountCfg["appSecret"] = req.AppSecret
		}
		accountCfg["enabled"] = true
		accountCfg["dmPolicy"] = req.DMPolicy
		accountCfg["groupPolicy"] = req.GroupPolicy
		accounts[req.Account] = accountCfg
		channelCfg["accounts"] = accounts
	case "whatsapp":
		// WhatsApp often requires QR/device login. Here we persist policy defaults.
	default:
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "unsupported channel: " + channel})
	}

	channels[channel] = channelCfg

	if err := bot.SetConfigMap(cfgMap); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to persist channel config"})
	}
	if err := model.UpdateBot(bot); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to save bot"})
	}

	if bot.Status == model.BotStatusRunning {
		if runtime.IsDockerPoolMode() {
			if err := runtime.SyncBotConfigSections(context.Background(), bot, "channels"); err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to apply channel config: " + err.Error()})
			}
		} else {
			if err := k8s.SyncSectionsToPod(context.Background(), bot.ID, "channels"); err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to sync channel config"})
			}
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"ok":      true,
		"channel": channelCfg,
		"summary": summarizeSingleChannel(channel, channelCfg),
	})
}

func deleteBotChannel(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := mustOwnBot(c.Param("id"), user)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]any{"ok": false, "message": err.Error()})
	}
	channel := strings.TrimSpace(strings.ToLower(c.Param("channel")))
	if channel == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "channel is required"})
	}

	cfgMap, err := bot.GetConfigMap()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to read bot config"})
	}
	channels := toMap(cfgMap["channels"])
	if channels == nil {
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	}
	delete(channels, channel)

	if err := bot.SetConfigMap(cfgMap); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to update channel config"})
	}
	if err := model.UpdateBot(bot); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to save bot"})
	}

	if bot.Status == model.BotStatusRunning {
		if runtime.IsDockerPoolMode() {
			if err := runtime.SyncBotConfigSections(context.Background(), bot, "channels"); err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to apply channel config: " + err.Error()})
			}
		} else {
			if err := k8s.SyncSectionsToPod(context.Background(), bot.ID, "channels"); err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to sync channel config"})
			}
		}
	}

	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

func testBotChannel(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := mustOwnBot(c.Param("id"), user)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]any{"ok": false, "message": err.Error()})
	}
	channel := strings.TrimSpace(strings.ToLower(c.Param("channel")))
	if channel == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "channel is required"})
	}
	account := strings.TrimSpace(c.QueryParam("account"))
	if account == "" {
		account = "default"
	}

	cfgMap, err := bot.GetConfigMap()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to read bot config"})
	}
	channels := toMap(cfgMap["channels"])
	if channels == nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "channel config not found"})
	}

	channelCfg := toMap(channels[channel])
	if channelCfg == nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "channel config not found"})
	}

	ok, message, details := runChannelConnectivityTest(channel, channelCfg, account)
	return c.JSON(http.StatusOK, map[string]any{
		"ok":      ok,
		"message": message,
		"details": details,
	})
}

func approveBotChannelPairing(c echo.Context) error {
	user, err := mustSessionUser(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]any{"ok": false, "message": "unauthorized"})
	}
	bot, err := mustOwnBot(c.Param("id"), user)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]any{"ok": false, "message": err.Error()})
	}
	channel := strings.TrimSpace(strings.ToLower(c.Param("channel")))
	if channel == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "channel is required"})
	}
	var req portalChannelPairingApproveRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "invalid request body"})
	}
	req.Code = strings.TrimSpace(req.Code)
	if req.Code == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "pairing code is required"})
	}
	if bot.Status != model.BotStatusRunning {
		return c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "message": "bot is not running"})
	}

	output, err := runtime.ApproveChannelPairing(context.Background(), bot, channel, req.Code)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "message": "failed to approve pairing: " + err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"ok":      true,
		"message": "pairing approved",
		"output":  output,
	})
}

func runChannelConnectivityTest(channel string, cfg map[string]interface{}, account string) (bool, string, map[string]any) {
	client := &http.Client{Timeout: 12 * time.Second}

	switch channel {
	case "telegram":
		botToken := stringFromMap(cfg, "botToken")
		if botToken == "" {
			return false, "telegram botToken is empty", nil
		}
		url := "https://api.telegram.org/bot" + botToken + "/getMe"
		resp, err := client.Get(url)
		if err != nil {
			return false, "telegram request failed: " + err.Error(), nil
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var data map[string]interface{}
		_ = json.Unmarshal(body, &data)
		ok, _ := data["ok"].(bool)
		if !ok {
			return false, "telegram getMe failed", data
		}
		return true, "telegram token is valid", data

	case "discord":
		token := strings.TrimSpace(stringFromMap(cfg, "token"))
		if token == "" {
			return false, "discord token is empty", nil
		}
		req, _ := http.NewRequest(http.MethodGet, "https://discord.com/api/v10/users/@me", nil)
		req.Header.Set("Authorization", "Bot "+token)
		resp, err := client.Do(req)
		if err != nil {
			return false, "discord request failed: " + err.Error(), nil
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var data map[string]interface{}
		_ = json.Unmarshal(body, &data)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return false, "discord auth failed", map[string]any{"status": resp.StatusCode, "response": data}
		}
		return true, "discord token is valid", map[string]any{"status": resp.StatusCode, "response": data}

	case "feishu":
		var appID, appSecret string
		accounts := toMap(cfg["accounts"])
		if len(accounts) > 0 {
			accountCfg := toMap(accounts[account])
			if accountCfg == nil {
				for _, raw := range accounts {
					accountCfg = toMap(raw)
					if accountCfg != nil {
						break
					}
				}
			}
			if accountCfg != nil {
				appID = stringFromMap(accountCfg, "appId")
				appSecret = stringFromMap(accountCfg, "appSecret")
			}
		}
		if appID == "" {
			appID = stringFromMap(cfg, "appId")
		}
		if appSecret == "" {
			appSecret = stringFromMap(cfg, "appSecret")
		}
		if appID == "" || appSecret == "" {
			return false, "feishu appId/appSecret is empty", nil
		}
		payload, _ := json.Marshal(map[string]string{
			"app_id":     appID,
			"app_secret": appSecret,
		})
		req, _ := http.NewRequest(http.MethodPost, "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return false, "feishu request failed: " + err.Error(), nil
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var data map[string]interface{}
		_ = json.Unmarshal(body, &data)
		code, _ := data["code"].(float64)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 || code != 0 {
			return false, "feishu credential validation failed", map[string]any{"status": resp.StatusCode, "response": data}
		}
		return true, "feishu credential is valid", map[string]any{"status": resp.StatusCode}

	case "whatsapp":
		return true, "whatsapp channel saved. QR/device pairing is required in OpenClaw runtime.", map[string]any{
			"hint": "open bot and complete whatsapp device pairing",
		}

	default:
		return false, "unsupported channel", nil
	}
}

func toMap(v interface{}) map[string]interface{} {
	if v == nil {
		return nil
	}
	switch m := v.(type) {
	case map[string]interface{}:
		return m
	default:
		return nil
	}
}

func ensureMapField(parent map[string]interface{}, key string) map[string]interface{} {
	if parent == nil {
		return map[string]interface{}{}
	}
	if existing := toMap(parent[key]); existing != nil {
		return existing
	}
	next := map[string]interface{}{}
	parent[key] = next
	return next
}

func normalizeStringList(input []string) []string {
	if len(input) == 0 {
		return nil
	}
	out := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, item := range input {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func buildChannelSummaries(channels map[string]interface{}) []portalChannelConfigResponse {
	out := make([]portalChannelConfigResponse, 0, len(channels))
	for name, raw := range channels {
		if cfg := summarizeSingleChannel(name, toMap(raw)); cfg != nil {
			out = append(out, *cfg)
		}
	}
	return out
}

func summarizeSingleChannel(channel string, cfg map[string]interface{}) *portalChannelConfigResponse {
	if cfg == nil {
		return nil
	}
	resp := &portalChannelConfigResponse{
		Provider:    channel,
		Enabled:     boolFromMap(cfg, "enabled"),
		DMPolicy:    stringFromMap(cfg, "dmPolicy"),
		GroupPolicy: stringFromMap(cfg, "groupPolicy"),
		AllowFrom:   stringSliceFromMap(cfg, "allowFrom"),
	}
	if v, ok := cfg["requireMention"].(bool); ok {
		resp.RequireMention = v
	}
	if token := stringFromMap(cfg, "botToken"); token != "" {
		resp.HasBotToken = true
	}
	if token := stringFromMap(cfg, "token"); token != "" {
		resp.HasToken = true
	}

	accounts := toMap(cfg["accounts"])
	if len(accounts) > 0 {
		for accountName, raw := range accounts {
			acc := toMap(raw)
			if acc == nil {
				continue
			}
			resp.Account = accountName
			if appID := stringFromMap(acc, "appId"); appID != "" {
				resp.AppID = appID
			}
			if appSecret := stringFromMap(acc, "appSecret"); appSecret != "" {
				resp.HasAppSecret = true
			}
			if dm := stringFromMap(acc, "dmPolicy"); dm != "" {
				resp.DMPolicy = dm
			}
			if gp := stringFromMap(acc, "groupPolicy"); gp != "" {
				resp.GroupPolicy = gp
			}
			break
		}
	}
	return resp
}

func stringSliceFromMap(m map[string]interface{}, key string) []string {
	if m == nil {
		return nil
	}
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil
	}
	var out []string
	switch v := raw.(type) {
	case []interface{}:
		out = make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				continue
			}
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
	case []string:
		out = make([]string, 0, len(v))
		for _, item := range v {
			item = strings.TrimSpace(item)
			if item != "" {
				out = append(out, item)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func stringFromMap(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key]
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func boolFromMap(m map[string]interface{}, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func intFromMap(m map[string]interface{}, key string) int {
	if m == nil {
		return 0
	}
	return intFromAny(m[key])
}

func intFromAny(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int8:
		return int(n)
	case int16:
		return int(n)
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float32:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
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
		if len(bots) > 1 {
			log.Printf("portal: detected %d bots for user=%s app=%s; using most recent bot=%s", len(bots), user.ID, appID, bots[0].ID)
		}
		bot = bots[0]
	}

	if bot.Status != model.BotStatusRunning {
		endpoint, err := portalRuntimeStartBot(context.Background(), bot, nil)
		if err != nil {
			return nil, err
		}
		if err := model.UpdateBotStatus(bot.ID, model.BotStatusRunning, endpoint); err != nil {
			_ = portalRuntimeRelease(bot.ID)
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
	health, ready := resolveBotHealth(bot)
	return portalBotResponse{
		ID:        bot.ID,
		Name:      bot.Name,
		Slug:      bot.Slug,
		Status:    bot.Status,
		Endpoint:  bot.Endpoint,
		AccessURL: buildAccessURL(bot),
		Health:    health,
		Ready:     ready,
	}
}

func resolveBotHealth(bot *model.Bot) (string, bool) {
	if bot == nil {
		return "unknown", false
	}
	if bot.Status != model.BotStatusRunning {
		return "stopped", false
	}
	if strings.TrimSpace(bot.Endpoint) == "" {
		return "no-endpoint", false
	}
	ready, err := runtime.GetBotReady(context.Background(), bot)
	if err != nil {
		return "error", false
	}
	if ready {
		return "ready", true
	}
	return "unreachable", false
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
      --bg: #f3f3f3;
      --bg-soft: #ececec;
      --card: #ffffff;
      --text: #141414;
      --muted: #666666;
      --line: #d8d8d8;
      --primary: #111111;
      --primary-hover: #2a2a2a;
      --danger: #8f1d1d;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: "IBM Plex Sans", "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
      background:
        linear-gradient(120deg, #fafafa 0%, var(--bg) 40%, var(--bg-soft) 100%);
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
      box-shadow: 0 10px 24px rgba(0, 0, 0, 0.06);
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
      background: #f8f8f8;
      color: var(--text);
      cursor: pointer;
      font-size: 14px;
      transition: all 0.15s ease;
    }
    button:hover { border-color: #bdbdbd; background: #f2f2f2; }
    button.primary {
      background: var(--primary);
      color: #fff;
      border-color: var(--primary);
    }
    button.primary:hover { background: var(--primary-hover); }
    button.danger {
      border-color: #e3b8b8;
      color: var(--danger);
      background: #fbf3f3;
    }
    input {
      border: 1px solid var(--line);
      border-radius: 10px;
      padding: 10px 12px;
      font-size: 14px;
      min-width: 200px;
    }
    select {
      border: 1px solid var(--line);
      border-radius: 10px;
      padding: 10px 12px;
      font-size: 14px;
      min-width: 200px;
      background: #fff;
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
    .secret-inline {
      display: flex;
      gap: 8px;
      align-items: center;
    }
    .secret-inline input {
      flex: 1;
      min-width: 0;
    }
    .secret-inline button {
      padding: 8px 10px;
      font-size: 12px;
      white-space: nowrap;
    }
    .ai-panel {
      margin-top: 12px;
      border-top: 1px dashed var(--line);
      padding-top: 12px;
    }
    .channels-panel {
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
      background: #fcfcfc;
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
      <p class="sub">支持邮箱注册/登录（无域名可用）与 Google 登录（可选）。登录后自动分配专属 OpenClaw，每个用户固定 1 个实例，可在实例内配置多平台 Channel 与多个 Agent。</p>
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
        <div class="meta" id="poolInfo"></div>
        <div class="meta" id="userHint"></div>
        <div class="row">
          <button id="createBotBtn" class="primary" onclick="createBot()">新增专属实例</button>
          <button onclick="refresh()">刷新</button>
          <button class="danger" onclick="logout()">退出登录</button>
        </div>
      </div>
    </div>

    <div id="manageCard" class="card hidden">
      <h2 style="margin-top:0">我的 OpenClaw 实例</h2>
      <div class="meta">实例配额：每个用户 1 个（固定）。如未分配，请点击上方“新增专属实例”。</div>
      <div id="bots" class="bots"></div>
    </div>
  </div>

  <script>
    let currentUser = null;
    let currentBots = [];
    let currentPool = null;
    let poolRetryTimer = null;
    let poolRetryDeadlineMs = 0;
    const poolRetryIntervalMs = 15000;

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

    function getWarnFromURL() {
      const params = new URLSearchParams(window.location.search || '');
      return (params.get('warn') || '').trim();
    }

    function clearWarnFromURL() {
      if (!window.history || !window.history.replaceState) return;
      const url = new URL(window.location.href);
      if (url.searchParams.has('warn')) {
        url.searchParams.delete('warn');
        window.history.replaceState({}, '', url.toString());
      }
    }

    function stopPoolRetryPolling() {
      if (poolRetryTimer) {
        clearInterval(poolRetryTimer);
        poolRetryTimer = null;
      }
      poolRetryDeadlineMs = 0;
    }

    function startPoolRetryPolling() {
      if (poolRetryTimer) return;
      poolRetryDeadlineMs = Date.now() + poolRetryIntervalMs;
      poolRetryTimer = setInterval(async () => {
        const hasBot = Array.isArray(currentBots) && currentBots.length > 0;
        const poolFull = !!(currentPool && currentPool.full);
        if (!currentUser || hasBot || !poolFull) {
          stopPoolRetryPolling();
          return;
        }
        if (Date.now() >= poolRetryDeadlineMs) {
          poolRetryDeadlineMs = Date.now() + poolRetryIntervalMs;
          await refresh();
        } else {
          renderPoolHint();
        }
      }, 1000);
    }

    function renderPoolHint() {
      const userHint = document.getElementById('userHint');
      if (!userHint) return;
      const hasBot = Array.isArray(currentBots) && currentBots.length > 0;
      const poolFull = !!(currentPool && currentPool.full);
      if (hasBot) {
        userHint.textContent = '每个用户仅允许 1 个专属实例；如需新建，请先删除当前实例。';
        userHint.style.color = '#444';
        return;
      }
      if (poolFull) {
        const remainMs = Math.max(0, poolRetryDeadlineMs - Date.now());
        const remainSec = Math.max(1, Math.ceil(remainMs / 1000));
        userHint.textContent = '当前资源池已满，系统将自动重试分配（' + remainSec + ' 秒后）。';
        userHint.style.color = '#8f1d1d';
        return;
      }
      userHint.textContent = '';
    }

    function render() {
      const authArea = document.getElementById('authArea');
      const userArea = document.getElementById('userArea');
      const manageCard = document.getElementById('manageCard');
      const userInfo = document.getElementById('userInfo');
      const poolInfo = document.getElementById('poolInfo');
      const userHint = document.getElementById('userHint');
      const createBotBtn = document.getElementById('createBotBtn');
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

      if (currentPool && currentPool.mode === 'docker_pool' && currentPool.total > 0) {
        poolInfo.textContent = '资源池：已占用 ' + currentPool.active + '/' + currentPool.capacity + '（容器总数 ' + currentPool.total + '），剩余 ' + currentPool.free + '；max_running_bots=' + currentPool.maxRunning;
      } else {
        poolInfo.textContent = '';
      }
      const hasBot = Array.isArray(currentBots) && currentBots.length > 0;
      const poolFull = !!(currentPool && currentPool.full);
      if (createBotBtn) {
        createBotBtn.disabled = hasBot || (poolFull && !hasBot);
      }
      if (poolFull && !hasBot) {
        startPoolRetryPolling();
      } else {
        stopPoolRetryPolling();
      }
      renderPoolHint();

      botsEl.innerHTML = '';
      for (const b of currentBots) {
        const div = document.createElement('div');
        div.className = 'bot';
        const safeId = b.id.replace(/[^a-zA-Z0-9_-]/g, '');
        div.innerHTML =
          '<h3>' + b.name + '</h3>' +
          '<div class="meta">Status: ' + b.status + ' | Slug: ' + b.slug + '</div>' +
          '<div class="meta">Endpoint: ' + (b.endpoint || '-') + '</div>' +
          '<div class="meta">Health: <span id="health-status-' + safeId + '">' + (b.health || 'unknown') + '</span></div>' +
          '<div class="row">' +
            '<a href="' + b.access_url + '" target="_blank"><button class="primary">Open</button></a>' +
            '<button onclick="startBot(\'' + b.id + '\')">Start</button>' +
            '<button onclick="stopBot(\'' + b.id + '\')">Stop</button>' +
            '<button class="danger" onclick="deleteBot(\'' + b.id + '\')">Delete</button>' +
            '<button onclick="checkBotHealth(\'' + safeId + '\', \'' + b.id + '\')">Health</button>' +
            '<button onclick="toggleAIConfig(\'' + safeId + '\', \'' + b.id + '\')">AI Config</button>' +
            '<button onclick="toggleChannelsConfig(\'' + safeId + '\', \'' + b.id + '\')">Channels</button>' +
          '</div>' +
          '<div class="meta">' + b.access_url + '</div>' +
          '<div id="aiWrap-' + safeId + '" class="ai-panel hidden">' +
            '<div class="row">' +
              '<div class="field"><label>Provider</label><input id="ai-provider-' + safeId + '" placeholder="custom / openai / google" /></div>' +
              '<div class="field"><label>Base URL</label><input id="ai-baseurl-' + safeId + '" placeholder="https://api.openai.com/v1" /></div>' +
              '<div class="field"><label>API Key</label><div class="secret-inline"><input id="ai-apikey-' + safeId + '" type="password" placeholder="sk-..." /><button id="ai-apikey-toggle-' + safeId + '" onclick="toggleSecret(\'ai-apikey-' + safeId + '\', \'ai-apikey-toggle-' + safeId + '\')" type="button">显示</button></div></div>' +
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
          '</div>' +
          '<div id="chWrap-' + safeId + '" class="channels-panel hidden">' +
            '<div class="row">' +
              '<div class="field">' +
                '<label>Channel</label>' +
                '<select id="ch-provider-' + safeId + '" onchange="loadChannelsConfig(\'' + safeId + '\', \'' + b.id + '\')">' +
                  '<option value="telegram">telegram</option>' +
                  '<option value="discord">discord</option>' +
                  '<option value="feishu">feishu</option>' +
                  '<option value="whatsapp">whatsapp</option>' +
                '</select>' +
              '</div>' +
              '<div class="field">' +
                '<label>Account</label>' +
                '<input id="ch-account-' + safeId + '" value="default" onblur="loadChannelsConfig(\'' + safeId + '\', \'' + b.id + '\')" />' +
              '</div>' +
              '<div class="field">' +
                '<label>DM Policy</label>' +
                '<select id="ch-dm-' + safeId + '">' +
                  '<option value="pairing">pairing</option>' +
                  '<option value="open">open</option>' +
                  '<option value="allowlist">allowlist</option>' +
                  '<option value="disabled">disabled</option>' +
                '</select>' +
              '</div>' +
              '<div class="field">' +
                '<label>Group Policy</label>' +
                '<select id="ch-group-' + safeId + '">' +
                  '<option value="open">open</option>' +
                  '<option value="allowlist">allowlist</option>' +
                  '<option value="disabled">disabled</option>' +
                '</select>' +
              '</div>' +
            '</div>' +
            '<div class="row">' +
              '<div class="field"><label>Telegram Bot Token</label><div class="secret-inline"><input id="ch-bottoken-' + safeId + '" type="password" placeholder="123456:ABC..." /><button id="ch-bottoken-toggle-' + safeId + '" onclick="toggleSecret(\'ch-bottoken-' + safeId + '\', \'ch-bottoken-toggle-' + safeId + '\')" type="button">显示</button></div></div>' +
              '<div class="field"><label>Discord Token</label><div class="secret-inline"><input id="ch-token-' + safeId + '" type="password" placeholder="discord token" /><button id="ch-token-toggle-' + safeId + '" onclick="toggleSecret(\'ch-token-' + safeId + '\', \'ch-token-toggle-' + safeId + '\')" type="button">显示</button></div></div>' +
            '</div>' +
            '<div class="row">' +
              '<div class="field"><label>Feishu App ID</label><input id="ch-appid-' + safeId + '" placeholder="cli_xxx" /></div>' +
              '<div class="field"><label>Feishu App Secret</label><div class="secret-inline"><input id="ch-appsecret-' + safeId + '" type="password" placeholder="secret" /><button id="ch-appsecret-toggle-' + safeId + '" onclick="toggleSecret(\'ch-appsecret-' + safeId + '\', \'ch-appsecret-toggle-' + safeId + '\')" type="button">显示</button></div></div>' +
            '</div>' +
            '<div class="row">' +
              '<div class="field"><label>Allowlist Users (comma separated)</label><input id="ch-allowfrom-' + safeId + '" placeholder="5055510476,@alice" /></div>' +
              '<div class="field"><label>Pairing Code</label><input id="ch-paircode-' + safeId + '" placeholder="47PDJP3D" /></div>' +
            '</div>' +
            '<div class="row">' +
              '<button class="primary" onclick="saveChannelConfig(\'' + safeId + '\', \'' + b.id + '\')">Save Channel</button>' +
              '<button onclick="testChannelConfig(\'' + safeId + '\', \'' + b.id + '\')">Test Channel</button>' +
              '<button onclick="approveChannelPairing(\'' + safeId + '\', \'' + b.id + '\')">Approve Pairing</button>' +
              '<button onclick="removeChannelConfig(\'' + safeId + '\', \'' + b.id + '\')">Remove Channel</button>' +
              '<button onclick="loadChannelsConfig(\'' + safeId + '\', \'' + b.id + '\')">Reload Channels</button>' +
              '<span id="ch-status-' + safeId + '" class="meta"></span>' +
            '</div>' +
            '<div id="ch-summary-' + safeId + '" class="meta"></div>' +
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
          currentPool = null;
        } else {
          currentUser = data.user;
          currentBots = data.bots || [];
          currentPool = data.pool || null;
        }
      } catch (e) {
        currentUser = null;
        currentBots = [];
        currentPool = null;
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
      setAuthMessage(data.warning ? ('登录成功，但实例自动分配失败：' + data.warning) : '登录成功');
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
      setAuthMessage(data.warning ? ('注册成功，但实例自动分配失败：' + data.warning) : '注册成功');
      await refresh();
    }

    async function createBot() {
      if (Array.isArray(currentBots) && currentBots.length > 0) {
        alert('每个用户仅允许 1 个专属实例；请先删除当前实例后再新建。');
        return;
      }
      const data = await api('/portal/api/bots', { method: 'POST', body: JSON.stringify({}) });
      if (!data.ok) {
        if (data.pool) currentPool = data.pool;
        alert(data.message || 'Create failed');
        render();
        return;
      }
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

    async function deleteBot(id) {
      if (!confirm('确认删除该 OpenClaw 实例？删除后可重新新增，配额上限仍为 1。')) {
        return;
      }
      const data = await api('/portal/api/bots/' + id, { method: 'DELETE' });
      if (!data.ok) {
        alert(data.message || 'Delete failed');
        return;
      }
      await refresh();
    }

    async function checkBotHealth(safeId, botId) {
      const data = await api('/portal/api/bots/' + botId + '/health');
      if (!data.ok || !data.health) {
        setHealthStatus(safeId, 'error');
        return;
      }
      setHealthStatus(safeId, data.health.status || 'unknown');
    }

    function setHealthStatus(safeId, status) {
      const el = document.getElementById('health-status-' + safeId);
      if (!el) return;
      el.textContent = status || 'unknown';
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
      setInput('ai-apikey-' + safeId, cfg.apiKey || '');
      setInput('ai-modelid-' + safeId, cfg.modelId || '');
      setInput('ai-modelname-' + safeId, cfg.modelName || '');
      setInput('ai-apitype-' + safeId, cfg.apiType || 'openai-completions');
      setInput('ai-auth-' + safeId, cfg.auth || 'api-key');
      setInput('ai-max-' + safeId, cfg.maxTokens || '');
      setInput('ai-context-' + safeId, cfg.contextWindow || '');
      setAIStatus(safeId, cfg.hasApiKey ? 'API Key loaded' : 'API Key not set');
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
      const cfg = data.config || {};
      setInput('ai-provider-' + safeId, cfg.provider || payload.provider || 'custom');
      setInput('ai-baseurl-' + safeId, cfg.baseUrl || payload.baseUrl || '');
      setInput('ai-apikey-' + safeId, cfg.apiKey || payload.apiKey || '');
      setInput('ai-modelid-' + safeId, cfg.modelId || payload.modelId || '');
      setInput('ai-modelname-' + safeId, cfg.modelName || payload.modelName || '');
      setInput('ai-apitype-' + safeId, cfg.apiType || payload.apiType || 'openai-completions');
      setInput('ai-auth-' + safeId, cfg.auth || payload.auth || 'api-key');
      setInput('ai-max-' + safeId, cfg.maxTokens || payload.maxTokens || '');
      setInput('ai-context-' + safeId, cfg.contextWindow || payload.contextWindow || '');
      setAIStatus(safeId, cfg.hasApiKey ? 'Saved (API Key stored)' : 'Saved');
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

    function toggleSecret(inputId, btnId) {
      const input = document.getElementById(inputId);
      const btn = document.getElementById(btnId);
      if (!input || !btn) return;
      const nextType = input.type === 'password' ? 'text' : 'password';
      input.type = nextType;
      btn.textContent = nextType === 'password' ? '显示' : '隐藏';
    }

    function setAIStatus(safeId, msg) {
      const el = document.getElementById('ai-status-' + safeId);
      if (!el) return;
      el.textContent = msg;
    }

    async function toggleChannelsConfig(safeId, botId) {
      const wrap = document.getElementById('chWrap-' + safeId);
      if (!wrap) return;
      if (!wrap.classList.contains('hidden')) {
        wrap.classList.add('hidden');
        return;
      }
      wrap.classList.remove('hidden');
      await loadChannelsConfig(safeId, botId);
    }

    async function loadChannelsConfig(safeId, botId) {
      const data = await api('/portal/api/bots/' + botId + '/channels');
      if (!data.ok) {
        setChannelStatus(safeId, 'Load failed: ' + (data.message || 'unknown error'));
        return;
      }

      const provider = getInput('ch-provider-' + safeId) || 'telegram';
      const channels = data.channels || {};
      const cfg = channels[provider] || {};
      const account = getInput('ch-account-' + safeId) || 'default';
      const accountCfg = cfg.accounts && cfg.accounts[account] ? cfg.accounts[account] : {};

      setInput('ch-dm-' + safeId, (cfg.dmPolicy || accountCfg.dmPolicy || 'pairing'));
      setInput('ch-group-' + safeId, (cfg.groupPolicy || accountCfg.groupPolicy || 'open'));
      setInput('ch-bottoken-' + safeId, cfg.botToken || accountCfg.botToken || '');
      setInput('ch-token-' + safeId, cfg.token || accountCfg.token || '');
      setInput('ch-appid-' + safeId, accountCfg.appId || '');
      setInput('ch-appsecret-' + safeId, accountCfg.appSecret || '');
      const allowFrom = Array.isArray(cfg.allowFrom) ? cfg.allowFrom : [];
      setInput('ch-allowfrom-' + safeId, allowFrom.join(','));
      setInput('ch-paircode-' + safeId, '');

      const summaries = data.summaries || [];
      setChannelsSummary(safeId, summaries);
      setChannelStatus(safeId, 'Loaded');
    }

    async function saveChannelConfig(safeId, botId) {
      const provider = getInput('ch-provider-' + safeId) || 'telegram';
      const payload = {
        provider,
        account: getInput('ch-account-' + safeId) || 'default',
        dmPolicy: getInput('ch-dm-' + safeId) || 'pairing',
        groupPolicy: getInput('ch-group-' + safeId) || 'open',
        botToken: getInput('ch-bottoken-' + safeId),
        token: getInput('ch-token-' + safeId),
        appId: getInput('ch-appid-' + safeId),
        appSecret: getInput('ch-appsecret-' + safeId),
        allowFrom: parseCSV(getInput('ch-allowfrom-' + safeId)),
        enabled: true
      };

      const data = await api('/portal/api/bots/' + botId + '/channels/' + provider, {
        method: 'PUT',
        body: JSON.stringify(payload)
      });
      if (!data.ok) {
        setChannelStatus(safeId, 'Save failed: ' + (data.message || 'unknown error'));
        return;
      }

      setChannelStatus(safeId, 'Channel saved');
      await loadChannelsConfig(safeId, botId);
    }

    async function approveChannelPairing(safeId, botId) {
      const provider = getInput('ch-provider-' + safeId) || 'telegram';
      const code = getInput('ch-paircode-' + safeId);
      if (!code) {
        setChannelStatus(safeId, 'Approve failed: pairing code is required');
        return;
      }
      const data = await api('/portal/api/bots/' + botId + '/channels/' + provider + '/pairing/approve', {
        method: 'POST',
        body: JSON.stringify({ code })
      });
      if (!data.ok) {
        setChannelStatus(safeId, 'Approve failed: ' + (data.message || 'unknown error'));
        return;
      }
      setInput('ch-paircode-' + safeId, '');
      setChannelStatus(safeId, 'Pairing approved');
      await loadChannelsConfig(safeId, botId);
    }

    async function removeChannelConfig(safeId, botId) {
      const provider = getInput('ch-provider-' + safeId) || 'telegram';
      const data = await api('/portal/api/bots/' + botId + '/channels/' + provider, { method: 'DELETE' });
      if (!data.ok) {
        setChannelStatus(safeId, 'Remove failed: ' + (data.message || 'unknown error'));
        return;
      }
      setChannelStatus(safeId, 'Channel removed');
      await loadChannelsConfig(safeId, botId);
    }

    async function testChannelConfig(safeId, botId) {
      const provider = getInput('ch-provider-' + safeId) || 'telegram';
      const account = getInput('ch-account-' + safeId) || 'default';
      const data = await api('/portal/api/bots/' + botId + '/channels/' + provider + '/test?account=' + encodeURIComponent(account), {
        method: 'POST'
      });
      if (!data.ok) {
        setChannelStatus(safeId, 'Test failed: ' + (data.message || 'unknown error'));
        return;
      }
      setChannelStatus(safeId, 'Test ok: ' + (data.message || 'success'));
    }

    function setChannelStatus(safeId, msg) {
      const el = document.getElementById('ch-status-' + safeId);
      if (!el) return;
      el.textContent = msg || '';
    }

    function parseCSV(input) {
      if (!input) return [];
      return input
        .split(',')
        .map(s => s.trim())
        .filter(Boolean);
    }

    function setChannelsSummary(safeId, summaries) {
      const el = document.getElementById('ch-summary-' + safeId);
      if (!el) return;
      if (!Array.isArray(summaries) || summaries.length === 0) {
        el.textContent = 'No channel configured';
        return;
      }
      const lines = summaries.map((s) => {
        const channel = s.provider || 'unknown';
        const enabled = s.enabled ? 'enabled' : 'disabled';
        const dm = s.dmPolicy || '-';
        const gp = s.groupPolicy || '-';
        const allowFrom = Array.isArray(s.allowFrom) && s.allowFrom.length > 0 ? (' allow=' + s.allowFrom.join(',')) : '';
        const account = s.account ? (' account=' + s.account) : '';
        const app = s.appId ? (' appId=' + s.appId) : '';
        const tokenFlags = ' token=' + (s.hasToken ? 'set' : 'empty') + ' botToken=' + (s.hasBotToken ? 'set' : 'empty') + ' appSecret=' + (s.hasAppSecret ? 'set' : 'empty');
        return channel + ' [' + enabled + '] dm=' + dm + ' group=' + gp + account + app + allowFrom + tokenFlags;
      });
      el.textContent = lines.join(' | ');
    }

    async function logout() {
      await api('/portal/auth/logout', { method: 'POST' });
      await refresh();
    }

    const warnMsg = getWarnFromURL();
    if (warnMsg) {
      setAuthMessage(warnMsg, true);
      clearWarnFromURL();
    }

    refresh();
  </script>
</body>
</html>`
