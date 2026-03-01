package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/fastclaw-ai/fastclaw/service/runtime"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/spf13/viper"
	"gorm.io/gorm"
)

// isUUID checks if a string is in UUID format
func isUUID(s string) bool {
	uuidRegex := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	return uuidRegex.MatchString(strings.ToLower(s))
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for development
	},
}

// pairingErrorResponse represents the NOT_PAIRED error from OpenClaw gateway
type pairingErrorResponse struct {
	Code    string `json:"code"`
	Details struct {
		RequestID string `json:"requestId"`
	} `json:"details"`
	Message string `json:"message"`
}

// isNotPairedResponse checks if a JSON body is a NOT_PAIRED error
func isNotPairedResponse(body []byte) bool {
	var resp pairingErrorResponse
	return json.Unmarshal(body, &resp) == nil && resp.Code == "NOT_PAIRED"
}

// --- Poller-based auto-approval ---
// When a request with a valid ?token= arrives (initial page load),
// we start a short-lived poller that repeatedly checks for pending devices.
// This handles the race condition where the WebUI JS creates pending device
// requests after the initial page has loaded.
// No token caching to avoid multi-tenancy issues — only the authenticated
// request triggers approval, and only for a limited window.

var (
	activePollersMu sync.Mutex
	activePollers   = make(map[string]bool) // botID -> polling in progress
)

// autoApprovePoller polls for pending devices and approves them over a short period.
func autoApprovePoller(botID, accessToken string) {
	// Prevent duplicate pollers for the same bot
	activePollersMu.Lock()
	if activePollers[botID] {
		activePollersMu.Unlock()
		return
	}
	activePollers[botID] = true
	activePollersMu.Unlock()

	defer func() {
		activePollersMu.Lock()
		delete(activePollers, botID)
		activePollersMu.Unlock()
	}()

	ctx := context.Background()
	// Poll every 2 seconds for ~16 seconds to catch newly pending devices
	for i := 0; i < 8; i++ {
		time.Sleep(2 * time.Second)
		if err := k8s.AutoApproveAllPending(ctx, botID, accessToken); err != nil {
			fmt.Printf("[AutoApprove] Poller error for bot %s: %v\n", botID, err)
		}
	}
}

// ProxyToBot proxies requests to the OpenClaw bot
// Path format: /proxy/{bot_id_or_slug}/*
func ProxyToBot(c echo.Context) error {
	botIdentifier := c.Param("bot_id")
	if botIdentifier == "" {
		return util.BadRequest(c, "bot_id or slug is required")
	}

	// Get bot info - determine if it's an ID (UUID format) or slug (short string)
	var bot *model.Bot
	var err error
	if isUUID(botIdentifier) {
		bot, err = model.GetBotByID(botIdentifier)
	} else {
		bot, err = model.GetBotBySlug(botIdentifier)
	}
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return util.NotFound(c, "bot not found")
		}
		return util.InternalError(c, "failed to get bot")
	}

	// Auto-approval: only when request carries a valid access token.
	// Start a poller to approve devices that become pending in the next ~16s
	// (from WebUI JS requests that follow the initial page load).
	accessToken := ""
	if !runtime.IsDockerPoolMode() {
		if token := c.QueryParam("token"); token != "" && token == bot.AccessToken {
			accessToken = bot.AccessToken
			go autoApprovePoller(bot.ID, accessToken)
		}
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	targetHost, err := runtime.GetBotEndpoint(context.Background(), bot)
	if err != nil {
		return util.InternalError(c, "failed to get bot endpoint")
	}
	if targetHost == "" {
		return util.NotFound(c, "bot endpoint not found")
	}

	// Get the remaining path after /proxy/{bot_id}
	remainingPath := c.Param("*")
	// WebSocket requests should not be redirected; proxy root directly.
	if isWebSocketRequest(c.Request()) {
		if remainingPath == "" {
			remainingPath = "/"
		} else if !strings.HasPrefix(remainingPath, "/") {
			remainingPath = "/" + remainingPath
		}
		return proxyWebSocket(c, targetHost, remainingPath, bot.ID, accessToken)
	}

	if remainingPath == "" {
		// Canonicalize bot root URL with trailing slash so relative assets resolve
		// under /proxy/{bot_id}/ instead of /proxy/.
		reqPath := c.Request().URL.Path
		if !strings.HasSuffix(reqPath, "/") {
			redirectURL := reqPath + "/"
			if rawQuery := c.QueryString(); rawQuery != "" {
				redirectURL += "?" + rawQuery
			}
			return c.Redirect(http.StatusMovedPermanently, redirectURL)
		}
		remainingPath = "/"
	} else if !strings.HasPrefix(remainingPath, "/") {
		remainingPath = "/" + remainingPath
	}

	// Regular HTTP proxy
	targetURL := &url.URL{
		Scheme: "http",
		Host:   targetHost,
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = targetHost
		req.URL.Path = remainingPath
		req.URL.RawQuery = c.QueryString()

		// Forward real client IP
		clientIP := c.RealIP()
		req.Header.Set("X-Real-IP", clientIP)
		if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
			req.Header.Set("X-Forwarded-For", xff+", "+clientIP)
		} else {
			req.Header.Set("X-Forwarded-For", clientIP)
		}
	}

	// Response hook:
	// 1) Inject OpenClaw client bootstrap into HTML so ws gateway uses /proxy/{bot_id}.
	// 2) Auto-approve NOT_PAIRED errors for non-docker-pool mode.
	botID := bot.ID
	botPathID := botIdentifier
	token := accessToken
	requestToken := c.QueryParam("token")
	injectedGatewayToken := requestToken
	if runtime.IsDockerPoolMode() {
		// docker_pool OpenClaw instances may have gateway auth token enabled.
		// Use configured pool gateway token instead of bot access token.
		if t := strings.TrimSpace(viper.GetString("docker_pool.gateway_token")); t != "" {
			injectedGatewayToken = t
		}
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		contentType := strings.ToLower(resp.Header.Get("Content-Type"))
		needHTMLInject := strings.Contains(contentType, "text/html")
		needPairCheck := token != "" && !runtime.IsDockerPoolMode() && resp.StatusCode >= 400

		if !needHTMLInject && !needPairCheck {
			return nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil
		}

		if needHTMLInject {
			body = injectOpenClawBootstrap(body, botPathID, injectedGatewayToken)
		}

		if needPairCheck && isNotPairedResponse(body) {
			fmt.Printf("[Proxy] NOT_PAIRED detected in HTTP response for bot %s, auto-approving...\n", botID)
			go func() {
				ctx := context.Background()
				if err := k8s.AutoApproveAllPending(ctx, botID, token); err != nil {
					fmt.Printf("[Proxy] Auto-approve (HTTP) failed for bot %s: %v\n", botID, err)
				}
			}()
		}

		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return nil
	}

	proxy.ServeHTTP(c.Response(), c.Request())
	return nil
}

func isWebSocketRequest(r *http.Request) bool {
	upgrade := strings.ToLower(r.Header.Get("Upgrade"))
	connection := strings.ToLower(r.Header.Get("Connection"))
	return strings.Contains(upgrade, "websocket") && strings.Contains(connection, "upgrade")
}

func injectOpenClawBootstrap(body []byte, botPathID, token string) []byte {
	html := string(body)
	pathLiteral := strconv.Quote("/proxy/" + botPathID)
	tokenLiteral := strconv.Quote(token)
	script := fmt.Sprintf(`<script>(function(){try{var key="openclaw.control.settings.v1";var current={};var raw=window.localStorage.getItem(key);if(raw){current=JSON.parse(raw)||{};}var proto=window.location.protocol==="https:"?"wss":"ws";current.gatewayUrl=proto+"://"+window.location.host+%s;if(%s!==""){current.token=%s;}window.localStorage.setItem(key,JSON.stringify(current));}catch(_e){}})();</script>`, pathLiteral, tokenLiteral, tokenLiteral)

	if strings.Contains(strings.ToLower(html), "</head>") {
		return []byte(strings.Replace(html, "</head>", script+"</head>", 1))
	}
	return append([]byte(script), body...)
}

// buildWSRequestHeaders builds the headers for the backend WebSocket connection
func buildWSRequestHeaders(c echo.Context, targetHost string) http.Header {
	requestHeader := http.Header{}
	// Set Origin to the target host to pass OpenClaw's origin check
	// OpenClaw doesn't support wildcard "*" in allowedOrigins
	requestHeader.Set("Origin", fmt.Sprintf("http://%s", targetHost))
	if protocol := c.Request().Header.Get("Sec-WebSocket-Protocol"); protocol != "" {
		requestHeader.Set("Sec-WebSocket-Protocol", protocol)
	}
	// Forward Authorization header for password auth
	if auth := c.Request().Header.Get("Authorization"); auth != "" {
		requestHeader.Set("Authorization", auth)
	}
	// Forward Cookie header (OpenClaw may use cookie for session)
	if cookie := c.Request().Header.Get("Cookie"); cookie != "" {
		requestHeader.Set("Cookie", cookie)
	}
	// Forward real client IP
	clientIP := c.RealIP()
	requestHeader.Set("X-Real-IP", clientIP)
	if xff := c.Request().Header.Get("X-Forwarded-For"); xff != "" {
		requestHeader.Set("X-Forwarded-For", xff+", "+clientIP)
	} else {
		requestHeader.Set("X-Forwarded-For", clientIP)
	}
	return requestHeader
}

func proxyWebSocket(c echo.Context, targetHost, path, botID, accessToken string) error {
	// Upgrade client connection
	clientConn, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer clientConn.Close()

	// Connect to backend WebSocket
	backendURL := url.URL{
		Scheme:   "ws",
		Host:     targetHost,
		Path:     path,
		RawQuery: c.QueryString(),
	}

	requestHeader := buildWSRequestHeaders(c, targetHost)

	// Dial backend with auto-approval retry for NOT_PAIRED errors
	backendConn, resp, err := websocket.DefaultDialer.Dial(backendURL.String(), requestHeader)

	// Handle NOT_PAIRED during WebSocket handshake (upgrade rejected with HTTP error)
	if err != nil && accessToken != "" && !runtime.IsDockerPoolMode() && resp != nil && resp.Body != nil {
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr == nil && isNotPairedResponse(body) {
			fmt.Printf("[Proxy] NOT_PAIRED detected during WS handshake for bot %s, auto-approving...\n", botID)

			// Approve all pending devices (synchronous - wait for completion before retry)
			ctx := context.Background()
			if approveErr := k8s.AutoApproveAllPending(ctx, botID, accessToken); approveErr != nil {
				fmt.Printf("[Proxy] Auto-approve (WS) failed for bot %s: %v\n", botID, approveErr)
			}

			// Retry WebSocket connection after approval
			backendConn, _, err = websocket.DefaultDialer.Dial(backendURL.String(), requestHeader)
			if err == nil {
				fmt.Printf("[Proxy] WS retry succeeded for bot %s after auto-approval\n", botID)
			}
		}
	}

	if err != nil {
		c.Logger().Errorf("WebSocket dial error: %v, url: %s", err, backendURL.String())
		return err
	}
	defer backendConn.Close()

	// Bidirectional message forwarding
	errCh := make(chan error, 2)

	// Client -> Backend
	go func() {
		for {
			msgType, msg, err := clientConn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			if err := backendConn.WriteMessage(msgType, msg); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Backend -> Client
	// If the WebSocket upgrade succeeded but NOT_PAIRED comes as a message,
	// detect it on the first message and trigger auto-approval in the background
	go func() {
		firstMessage := true
		for {
			msgType, msg, err := backendConn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}

			// Check first message for NOT_PAIRED (handles the case where
			// WebSocket upgrade succeeds but pairing is checked at message level)
			if firstMessage && accessToken != "" && !runtime.IsDockerPoolMode() {
				firstMessage = false
				if isNotPairedResponse(msg) {
					fmt.Printf("[Proxy] NOT_PAIRED detected in WS message for bot %s, auto-approving...\n", botID)
					go func() {
						ctx := context.Background()
						if err := k8s.AutoApproveAllPending(ctx, botID, accessToken); err != nil {
							fmt.Printf("[Proxy] Auto-approve (WS msg) failed for bot %s: %v\n", botID, err)
						}
					}()
				}
			}

			if err := clientConn.WriteMessage(msgType, msg); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Wait for either direction to close
	<-errCh
	return nil
}
