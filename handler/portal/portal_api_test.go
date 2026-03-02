package portal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/labstack/echo/v4"
	"github.com/spf13/viper"
)

func restorePortalMocks() func() {
	origMustSession := portalMustSessionUser
	origEnsure := portalEnsureUserBotRunning
	origMustOwn := portalMustOwnBot
	origDelete := portalDeleteBotRecord
	origStop := portalRuntimeStopBot
	origRelease := portalRuntimeRelease

	return func() {
		portalMustSessionUser = origMustSession
		portalEnsureUserBotRunning = origEnsure
		portalMustOwnBot = origMustOwn
		portalDeleteBotRecord = origDelete
		portalRuntimeStopBot = origStop
		portalRuntimeRelease = origRelease
	}
}

func parseBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response failed: %v; body=%s", err, rec.Body.String())
	}
	return body
}

func TestCreateBotOneUserOneInstance(t *testing.T) {
	defer restorePortalMocks()()
	viper.Set("runtime.mode", "docker_pool")
	viper.Set("runtime.max_running_bots", 4)
	viper.Set("docker_pool.endpoints", []string{"openclaw-1:18789", "openclaw-2:18789"})

	user := &model.PortalUser{ID: "u1", Email: "u1@example.com"}
	created := &model.Bot{ID: "b-1", Name: "mybot", Slug: "slug1", Status: model.BotStatusRunning}
	portalMustSessionUser = func(c echo.Context) (*model.PortalUser, error) {
		return user, nil
	}
	portalEnsureUserBotRunning = func(u *model.PortalUser) (*model.Bot, error) {
		return created, nil
	}

	e := echo.New()
	req1 := httptest.NewRequest(http.MethodPost, "/portal/api/bots", strings.NewReader(`{}`))
	req1.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec1 := httptest.NewRecorder()
	if err := createBot(e.NewContext(req1, rec1)); err != nil {
		t.Fatalf("createBot #1 error: %v", err)
	}
	if rec1.Code != http.StatusOK {
		t.Fatalf("createBot #1 status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	body1 := parseBody(t, rec1)
	bot1 := body1["bot"].(map[string]any)

	req2 := httptest.NewRequest(http.MethodPost, "/portal/api/bots", strings.NewReader(`{}`))
	req2.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec2 := httptest.NewRecorder()
	if err := createBot(e.NewContext(req2, rec2)); err != nil {
		t.Fatalf("createBot #2 error: %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("createBot #2 status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	body2 := parseBody(t, rec2)
	bot2 := body2["bot"].(map[string]any)

	if bot1["id"] != bot2["id"] {
		t.Fatalf("expected same bot id for same user, got %v vs %v", bot1["id"], bot2["id"])
	}
}

func TestCreateBotPoolFullReturns409(t *testing.T) {
	defer restorePortalMocks()()
	viper.Set("runtime.mode", "docker_pool")
	viper.Set("runtime.max_running_bots", 1)
	viper.Set("docker_pool.endpoints", []string{"openclaw-1:18789"})

	portalMustSessionUser = func(c echo.Context) (*model.PortalUser, error) {
		return &model.PortalUser{ID: "u1", Email: "u1@example.com"}, nil
	}
	portalEnsureUserBotRunning = func(u *model.PortalUser) (*model.Bot, error) {
		return nil, errors.New("max running bots limit reached")
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/portal/api/bots", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	if err := createBot(e.NewContext(req, rec)); err != nil {
		t.Fatalf("createBot error: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := parseBody(t, rec)
	if body["ok"] != false {
		t.Fatalf("expected ok=false, body=%v", body)
	}
	if !strings.Contains(body["message"].(string), "资源池已满") {
		t.Fatalf("expected pool full message, got %v", body["message"])
	}
}

func TestDeleteThenCreateRecreate(t *testing.T) {
	defer restorePortalMocks()()
	user := &model.PortalUser{ID: "u1", Email: "u1@example.com"}
	currentBot := &model.Bot{ID: "b-old", Name: "old", Slug: "oldslug", Status: model.BotStatusRunning}
	deleted := false

	portalMustSessionUser = func(c echo.Context) (*model.PortalUser, error) { return user, nil }
	portalMustOwnBot = func(botID string, u *model.PortalUser) (*model.Bot, error) {
		return currentBot, nil
	}
	portalRuntimeStopBot = func(_ context.Context, _ *model.Bot) error { return nil }
	portalRuntimeRelease = func(_ string) error { return nil }
	portalDeleteBotRecord = func(id string) error {
		deleted = true
		currentBot = nil
		return nil
	}
	portalEnsureUserBotRunning = func(u *model.PortalUser) (*model.Bot, error) {
		if currentBot != nil {
			return currentBot, nil
		}
		currentBot = &model.Bot{ID: "b-new", Name: "new", Slug: "newslug", Status: model.BotStatusRunning}
		return currentBot, nil
	}

	e := echo.New()
	delReq := httptest.NewRequest(http.MethodDelete, "/portal/api/bots/b-old", nil)
	delRec := httptest.NewRecorder()
	delCtx := e.NewContext(delReq, delRec)
	delCtx.SetPath("/portal/api/bots/:id")
	delCtx.SetParamNames("id")
	delCtx.SetParamValues("b-old")
	if err := deleteBot(delCtx); err != nil {
		t.Fatalf("deleteBot error: %v", err)
	}
	if delRec.Code != http.StatusOK || !deleted {
		t.Fatalf("delete failed status=%d body=%s deleted=%v", delRec.Code, delRec.Body.String(), deleted)
	}

	createReq := httptest.NewRequest(http.MethodPost, "/portal/api/bots", strings.NewReader(`{}`))
	createReq.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	createRec := httptest.NewRecorder()
	if err := createBot(e.NewContext(createReq, createRec)); err != nil {
		t.Fatalf("createBot error: %v", err)
	}
	if createRec.Code != http.StatusOK {
		t.Fatalf("create after delete status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	body := parseBody(t, createRec)
	bot := body["bot"].(map[string]any)
	if bot["id"] != "b-new" {
		t.Fatalf("expected recreated bot id b-new, got %v", bot["id"])
	}
}

func TestCreateBotUnauthorized(t *testing.T) {
	defer restorePortalMocks()()
	portalMustSessionUser = func(c echo.Context) (*model.PortalUser, error) {
		return nil, errors.New("missing session")
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/portal/api/bots", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	if err := createBot(e.NewContext(req, rec)); err != nil {
		t.Fatalf("createBot error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d body=%s", rec.Code, rec.Body.String())
	}
}
