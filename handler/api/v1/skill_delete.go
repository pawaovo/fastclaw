package v1

import (
	"context"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/middleware"
	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
)

func DeleteSkill(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	name := c.Param("name")
	if name == "" {
		return util.BadRequest(c, "skill name is required")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running, cannot delete skills")
	}

	ctx := context.Background()

	// Delete skill directory in pod
	if err := deleteSkillFromPod(ctx, bot.ID, name); err != nil {
		return util.InternalError(c, "failed to delete skill: "+err.Error())
	}

	return util.Success(c, map[string]string{
		"message": "skill deleted",
		"name":    name,
	})
}

func deleteSkillFromPod(ctx context.Context, botID, skillName string) error {
	client := k8s.GetClient()
	namespace := k8s.GetNamespace()

	// Get pod name
	deploymentName := k8s.GetDeploymentName(botID)
	pods, err := client.CoreV1().Pods(namespace).List(ctx, k8s.ListOptions(deploymentName))
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return fmt.Errorf("no running pod found")
	}

	podName := pods.Items[0].Name
	skillPath := fmt.Sprintf("/app/.openclaw/workspace/skills/%s", skillName)

	// Remove directory
	_, err = k8s.ExecInPod(ctx, namespace, podName, "openclaw", []string{"rm", "-rf", skillPath})
	if err != nil {
		return fmt.Errorf("failed to delete skill directory: %w", err)
	}

	return nil
}
