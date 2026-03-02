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

type UpdateSkillRequest struct {
	Content string `json:"content" validate:"required"`
}

func UpdateSkill(c echo.Context) error {
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

	var req UpdateSkillRequest
	if err := c.Bind(&req); err != nil {
		return util.BadRequest(c, "invalid request body")
	}

	if req.Content == "" {
		return util.BadRequest(c, "content is required")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running, cannot update skills")
	}

	ctx := context.Background()

	// Write skill file to pod
	if err := writeSkillToPod(ctx, bot.ID, name, req.Content); err != nil {
		return util.InternalError(c, "failed to write skill: "+err.Error())
	}

	return util.Success(c, map[string]string{
		"message": "skill updated",
		"name":    name,
	})
}

func writeSkillToPod(ctx context.Context, botID, skillName, content string) error {
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

	// Create directory
	_, err = k8s.ExecInPod(ctx, namespace, podName, "openclaw", []string{"mkdir", "-p", skillPath})
	if err != nil {
		return fmt.Errorf("failed to create skill directory: %w", err)
	}

	// Write SKILL.md file
	// Using echo with heredoc style
	cmd := []string{"sh", "-c", fmt.Sprintf("cat > %s/SKILL.md << 'SKILLEOF'\n%s\nSKILLEOF", skillPath, content)}
	_, err = k8s.ExecInPod(ctx, namespace, podName, "openclaw", cmd)
	if err != nil {
		return fmt.Errorf("failed to write skill file: %w", err)
	}

	return nil
}
