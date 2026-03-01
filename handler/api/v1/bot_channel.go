package v1

import (
	"context"

	"github.com/fastclaw-ai/fastclaw/middleware"
	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
)

type AddChannelRequest struct {
	Channel string `json:"channel"`           // telegram, discord, slack, whatsapp, feishu, etc.
	Account string `json:"account,omitempty"` // Account name for multi-account support (default: "default")
	// Bot token - support both formats for compatibility
	BotToken string `json:"botToken,omitempty"` // Primary: botToken (telegram, discord, slack)
	Token    string `json:"token,omitempty"`    // Alias: token (legacy format)
	// Slack specific
	AppToken string `json:"appToken,omitempty"` // Slack app token (xapp-...)
	// Feishu specific
	AppID     string `json:"appId,omitempty"`     // Feishu app ID
	AppSecret string `json:"appSecret,omitempty"` // Feishu app secret
	// Teams specific
	AppPassword string `json:"appPassword,omitempty"` // Teams app password
	// LINE specific
	ChannelSecret string `json:"channelSecret,omitempty"` // LINE channel secret
	// Additional channel config options (dmPolicy, groupPolicy, allowFrom, enabled, etc.)
	DMPolicy    string   `json:"dmPolicy,omitempty"`    // pairing, allowlist, open, disabled
	GroupPolicy string   `json:"groupPolicy,omitempty"` // open, allowlist, disabled
	AllowFrom   []string `json:"allowFrom,omitempty"`   // List of allowed users/groups
	Enabled     *bool    `json:"enabled,omitempty"`     // Enable/disable channel
	// Extra config for any other fields
	Extra map[string]interface{} `json:"extra,omitempty"`
}

// AddChannel adds an IM channel to a bot
// POST /bots/:id/channels
func AddChannel(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	var req AddChannelRequest
	if err := c.Bind(&req); err != nil {
		return util.BadRequest(c, "invalid request body")
	}

	if req.Channel == "" {
		return util.BadRequest(c, "channel is required")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	// Default account name
	account := req.Account
	if account == "" {
		account = "default"
	}

	// Get bot token - prefer botToken, fallback to token
	botToken := req.BotToken
	if botToken == "" {
		botToken = req.Token
	}

	// Build config map from request fields
	configMap := make(map[string]interface{})
	// Token fields
	if botToken != "" {
		configMap["botToken"] = botToken
	}
	if req.AppToken != "" {
		configMap["appToken"] = req.AppToken
	}
	if req.AppID != "" {
		configMap["appId"] = req.AppID
	}
	if req.AppSecret != "" {
		configMap["appSecret"] = req.AppSecret
	}
	if req.AppPassword != "" {
		configMap["appPassword"] = req.AppPassword
	}
	if req.ChannelSecret != "" {
		configMap["channelSecret"] = req.ChannelSecret
	}
	// Policy fields
	if req.DMPolicy != "" {
		configMap["dmPolicy"] = req.DMPolicy
	}
	if req.GroupPolicy != "" {
		configMap["groupPolicy"] = req.GroupPolicy
	}
	if len(req.AllowFrom) > 0 {
		configMap["allowFrom"] = req.AllowFrom
	}
	if req.Enabled != nil {
		configMap["enabled"] = *req.Enabled
	}
	// Merge extra config
	for k, v := range req.Extra {
		configMap[k] = v
	}

	// Add channel to the running pod
	if err := k8s.AddChannelToBot(context.Background(), bot.ID, bot.AccessToken, req.Channel, account, configMap); err != nil {
		return util.InternalError(c, "failed to add channel: "+err.Error())
	}

	return util.Success(c, map[string]interface{}{
		"message": "channel added successfully",
		"channel": req.Channel,
		"account": account,
	})
}

// ListChannels lists all channels for a bot
// GET /bots/:id/channels
func ListChannels(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	// Get channels from the running pod
	channels, err := k8s.ListBotChannels(context.Background(), bot.ID, bot.AccessToken)
	if err != nil {
		return util.InternalError(c, "failed to list channels: "+err.Error())
	}

	return util.Success(c, channels)
}

// RemoveChannel removes an IM channel or specific account from a bot
// DELETE /bots/:id/channels/:channel?account=xxx
// If account query param is provided, removes only that account; otherwise removes entire channel
func RemoveChannel(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	channel := c.Param("channel")
	account := c.QueryParam("account") // Optional: specific account to remove

	if channel == "" {
		return util.BadRequest(c, "channel is required")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	// Remove channel or account from the running pod
	if err := k8s.RemoveChannelFromBot(context.Background(), bot.ID, bot.AccessToken, channel, account); err != nil {
		return util.InternalError(c, "failed to remove channel: "+err.Error())
	}

	result := map[string]string{
		"message": "channel removed successfully",
		"channel": channel,
	}
	if account != "" {
		result["account"] = account
		result["message"] = "account removed successfully"
	}

	return util.Success(c, result)
}

// ChannelPairingApproveRequest represents a channel pairing approval request
type ChannelPairingApproveRequest struct {
	Code string `json:"code"` // Pairing code from the channel (e.g., "JDB55KTQ")
}

// ChannelPairingRevokeRequest represents a channel pairing revoke request
type ChannelPairingRevokeRequest struct {
	UserID string `json:"user_id"` // User ID to revoke (e.g., "1743739674")
}

// ApproveChannelPairing approves a channel pairing request
// POST /bots/:id/channels/:channel/pairing/approve
func ApproveChannelPairing(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	channel := c.Param("channel")
	if channel == "" {
		return util.BadRequest(c, "channel is required")
	}

	var req ChannelPairingApproveRequest
	if err := c.Bind(&req); err != nil {
		return util.BadRequest(c, "invalid request body")
	}

	if req.Code == "" {
		return util.BadRequest(c, "pairing code is required")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	// Approve channel pairing
	output, err := k8s.ApproveChannelPairing(context.Background(), bot.ID, channel, req.Code)
	if err != nil {
		return util.InternalError(c, "failed to approve pairing: "+err.Error())
	}

	return util.Success(c, map[string]interface{}{
		"message": "pairing approved successfully",
		"channel": channel,
		"code":    req.Code,
		"output":  output,
	})
}

// RevokeChannelPairing revokes a channel pairing for a user
// POST /bots/:id/channels/:channel/pairing/revoke
func RevokeChannelPairing(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	channel := c.Param("channel")
	if channel == "" {
		return util.BadRequest(c, "channel is required")
	}

	var req ChannelPairingRevokeRequest
	if err := c.Bind(&req); err != nil {
		return util.BadRequest(c, "invalid request body")
	}

	if req.UserID == "" {
		return util.BadRequest(c, "user_id is required")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	// Revoke channel pairing
	output, err := k8s.RevokeChannelPairing(context.Background(), bot.ID, channel, req.UserID)
	if err != nil {
		return util.InternalError(c, "failed to revoke pairing: "+err.Error())
	}

	return util.Success(c, map[string]interface{}{
		"message": "pairing revoked successfully",
		"channel": channel,
		"user_id": req.UserID,
		"output":  output,
	})
}

// GetChannelPairedUsers lists all paired users for a channel
// GET /bots/:id/channels/:channel/pairing/users
func GetChannelPairedUsers(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	channel := c.Param("channel")
	if channel == "" {
		return util.BadRequest(c, "channel is required")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	// Get paired users from config (allowFrom list)
	users, err := k8s.GetChannelPairedUsers(context.Background(), bot.ID, channel)
	if err != nil {
		return util.InternalError(c, "failed to get paired users: "+err.Error())
	}

	return util.Success(c, map[string]interface{}{
		"channel": channel,
		"users":   users,
	})
}

// ListChannelPairingRequests lists pending pairing requests for a channel
// GET /bots/:id/channels/:channel/pairing
func ListChannelPairingRequests(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	channel := c.Param("channel")
	if channel == "" {
		return util.BadRequest(c, "channel is required")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	// List channel pairing requests
	response, err := k8s.ListChannelPairingRequests(context.Background(), bot.ID, channel)
	if err != nil {
		return util.InternalError(c, "failed to list pairing requests: "+err.Error())
	}

	return util.Success(c, response)
}
