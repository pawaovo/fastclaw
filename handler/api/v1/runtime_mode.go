package v1

import (
	"github.com/fastclaw-ai/fastclaw/service/runtime"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
)

func ensureK8sMode(c echo.Context) error {
	if runtime.IsDockerPoolMode() {
		return util.BadRequest(c, "this API is not supported in docker_pool mode")
	}
	return nil
}
