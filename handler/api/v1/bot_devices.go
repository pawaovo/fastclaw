package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fastclaw-ai/fastclaw/middleware"
	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
)

// DeviceInfo represents a device in the pairing list
type DeviceInfo struct {
	RequestID  string `json:"request_id,omitempty"`
	DeviceID   string `json:"device_id"`
	Role       string `json:"role,omitempty"`
	Platform   string `json:"platform,omitempty"`
	ClientID   string `json:"client_id,omitempty"`
	ClientMode string `json:"client_mode,omitempty"`
	IP         string `json:"ip,omitempty"`
	Age        string `json:"age,omitempty"`
	Revoked    bool   `json:"revoked,omitempty"`
	Connected  bool   `json:"connected,omitempty"`
	Status     string `json:"status"` // pending, paired, or revoked
}

// openclawDeviceList represents the JSON output from openclaw devices list --json
type openclawDeviceList struct {
	Pending []openclawPendingDevice `json:"pending"`
	Paired  []openclawPairedDevice  `json:"paired"`
}

type openclawPendingDevice struct {
	RequestID  string `json:"requestId"`
	DeviceID   string `json:"deviceId"`
	Role       string `json:"role"`
	Platform   string `json:"platform"`
	ClientID   string `json:"clientId"`
	ClientMode string `json:"clientMode"`
	IP         string `json:"ip"`
	Ts         int64  `json:"ts"` // timestamp in ms
}

type openclawPairedDevice struct {
	DeviceID     string                `json:"deviceId"`
	Role         string                `json:"role"`
	Platform     string                `json:"platform"`
	ClientID     string                `json:"clientId"`
	ClientMode   string                `json:"clientMode"`
	CreatedAtMs  int64                 `json:"createdAtMs"`
	ApprovedAtMs int64                 `json:"approvedAtMs"`
	Tokens       []openclawDeviceToken `json:"tokens"`
}

type openclawDeviceToken struct {
	Role        string `json:"role"`
	RevokedAtMs int64  `json:"revokedAtMs,omitempty"`
}

// formatAge formats a duration as a human-readable age string
func formatAge(ms int64) string {
	if ms == 0 {
		return ""
	}
	d := time.Since(time.UnixMilli(ms))
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	} else if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	} else if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// ListDevices returns the list of pending and paired devices for a bot
// Query params:
//   - status: filter by status ("pending" or "paired"), default returns all
//   - client_mode: filter by client mode ("web", "cli", "desktop", etc.), default returns all
func ListDevices(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	statusFilter := c.QueryParam("status")          // "pending", "paired", or empty for all
	clientModeFilter := c.QueryParam("client_mode") // "web", "cli", "desktop", etc.

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	ctx := context.Background()

	// Try Gateway WebSocket API first (faster)
	devices, err := listDevicesViaGateway(ctx, bot)
	if err != nil {
		// Fallback to CLI method
		c.Logger().Warnf("Gateway API failed (%v), falling back to CLI", err)
		devices, err = listDevicesViaCLI(ctx, bot)
		if err != nil {
			c.Logger().Errorf("CLI fallback also failed: %v", err)
			return util.InternalError(c, "failed to list devices: "+err.Error())
		}
		c.Logger().Info("CLI fallback succeeded")
	}

	// Ensure devices is never nil (return empty array instead of null)
	if devices == nil {
		devices = []DeviceInfo{}
	}

	// Filter by status if specified
	if statusFilter != "" {
		filtered := []DeviceInfo{}
		for _, d := range devices {
			if d.Status == statusFilter {
				filtered = append(filtered, d)
			}
		}
		devices = filtered
	}

	// Filter by client_mode if specified
	if clientModeFilter != "" {
		filtered := []DeviceInfo{}
		for _, d := range devices {
			if d.ClientMode == clientModeFilter {
				filtered = append(filtered, d)
			}
		}
		devices = filtered
	}

	return util.Success(c, map[string]interface{}{
		"bot_id":  bot.ID,
		"devices": devices,
	})
}

// listDevicesViaGateway uses the Gateway WebSocket API to list devices (fast)
func listDevicesViaGateway(ctx context.Context, bot *model.Bot) ([]DeviceInfo, error) {
	result, err := k8s.ListBotDevicesViaGateway(ctx, bot.ID, bot.AccessToken)
	if err != nil {
		return nil, err
	}

	devices := []DeviceInfo{}

	// Convert pending requests
	for _, d := range result.Pending {
		devices = append(devices, DeviceInfo{
			RequestID:  d.RequestID,
			DeviceID:   d.DeviceID,
			Role:       d.Role,
			Platform:   d.Platform,
			ClientID:   d.ClientID,
			ClientMode: d.ClientMode,
			IP:         d.IP,
			Age:        formatAge(d.Ts),
			Status:     "pending",
		})
	}

	// Convert paired nodes
	for _, d := range result.Paired {
		// Check if device is revoked (all tokens revoked)
		revoked := false
		if len(d.Tokens) > 0 {
			allRevoked := true
			for _, t := range d.Tokens {
				if t.RevokedAtMs == 0 {
					allRevoked = false
					break
				}
			}
			revoked = allRevoked
		}

		status := "paired"
		if revoked {
			status = "revoked"
		}

		devices = append(devices, DeviceInfo{
			DeviceID:   d.DeviceID,
			Role:       d.Role,
			Platform:   d.Platform,
			ClientID:   d.ClientID,
			ClientMode: d.ClientMode,
			Age:        formatAge(d.ApprovedAtMs),
			Revoked:    revoked,
			Connected:  d.Connected,
			Status:     status,
		})
	}

	return devices, nil
}

// listDevicesViaCLI uses CLI command to list devices (slower, fallback)
func listDevicesViaCLI(ctx context.Context, bot *model.Bot) ([]DeviceInfo, error) {
	// Get pod name
	podName, err := k8s.GetPodName(ctx, bot.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod: %w", err)
	}

	// Execute devices list command with --json flag and token for gateway auth
	output, err := k8s.ExecInPod(ctx, k8s.GetNamespace(), podName, "openclaw",
		[]string{"node", "/app/openclaw.mjs", "devices", "list", "--json", "--token", bot.AccessToken})
	if err != nil {
		return nil, fmt.Errorf("failed to execute CLI: %w", err)
	}

	// Parse JSON output
	var deviceList openclawDeviceList
	if err := json.Unmarshal([]byte(output), &deviceList); err != nil {
		return nil, fmt.Errorf("failed to parse devices: %w", err)
	}

	// Convert to DeviceInfo
	devices := []DeviceInfo{}
	for _, d := range deviceList.Pending {
		devices = append(devices, DeviceInfo{
			RequestID:  d.RequestID,
			DeviceID:   d.DeviceID,
			Role:       d.Role,
			Platform:   d.Platform,
			ClientID:   d.ClientID,
			ClientMode: d.ClientMode,
			IP:         d.IP,
			Age:        formatAge(d.Ts),
			Status:     "pending",
		})
	}
	for _, d := range deviceList.Paired {
		// Check if device is revoked (all tokens revoked)
		revoked := false
		if len(d.Tokens) > 0 {
			allRevoked := true
			for _, t := range d.Tokens {
				if t.RevokedAtMs == 0 {
					allRevoked = false
					break
				}
			}
			revoked = allRevoked
		}

		status := "paired"
		if revoked {
			status = "revoked"
		}

		devices = append(devices, DeviceInfo{
			DeviceID:   d.DeviceID,
			Role:       d.Role,
			Platform:   d.Platform,
			ClientID:   d.ClientID,
			ClientMode: d.ClientMode,
			Age:        formatAge(d.ApprovedAtMs),
			Revoked:    revoked,
			Status:     status,
		})
	}

	return devices, nil
}

// ApproveDevice approves a pending device pairing request
func ApproveDevice(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	requestID := c.Param("request_id")
	if requestID == "" {
		return util.BadRequest(c, "request_id is required")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	ctx := context.Background()

	// Use Gateway WebSocket API directly (bypasses CLI wss:// security check)
	endpoint, err := k8s.GetServiceEndpoint(ctx, bot.ID)
	if err != nil {
		return util.InternalError(c, "failed to get service endpoint: "+err.Error())
	}

	client, err := k8s.NewGatewayClient(ctx, endpoint, bot.AccessToken)
	if err != nil {
		return util.InternalError(c, "failed to connect to gateway: "+err.Error())
	}
	defer client.Close()

	if err := client.ApprovePairRequest(ctx, requestID); err != nil {
		return util.InternalError(c, "failed to approve device: "+err.Error())
	}

	return util.Success(c, map[string]interface{}{
		"bot_id":     bot.ID,
		"request_id": requestID,
		"message":    "device approved",
	})
}

// RevokeDevice revokes a paired device
// Query params:
//   - role: the role to revoke (default: "user")
func RevokeDevice(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	deviceID := c.Param("device_id")
	role := c.QueryParam("role")
	if role == "" {
		role = "operator" // default role
	}

	if deviceID == "" {
		return util.BadRequest(c, "device_id is required")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	ctx := context.Background()

	// Use Gateway WebSocket API directly (bypasses CLI wss:// security check)
	endpoint, err := k8s.GetServiceEndpoint(ctx, bot.ID)
	if err != nil {
		return util.InternalError(c, "failed to get service endpoint: "+err.Error())
	}

	client, err := k8s.NewGatewayClient(ctx, endpoint, bot.AccessToken)
	if err != nil {
		return util.InternalError(c, "failed to connect to gateway: "+err.Error())
	}
	defer client.Close()

	if err := client.RevokeNode(ctx, deviceID, role); err != nil {
		return util.InternalError(c, "failed to revoke device: "+err.Error())
	}

	return util.Success(c, map[string]interface{}{
		"bot_id":    bot.ID,
		"device_id": deviceID,
		"role":      role,
		"message":   "device revoked",
	})
}
