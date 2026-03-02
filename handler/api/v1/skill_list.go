package v1

import (
	"context"
	"fmt"
	"strings"

	"github.com/fastclaw-ai/fastclaw/middleware"
	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
)

type SkillInfo struct {
	Name string `json:"name"`
}

func ListSkills(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running, cannot list skills")
	}

	ctx := context.Background()

	// List skills directory in pod
	skills, err := listSkillsInPod(ctx, bot.ID)
	if err != nil {
		return util.InternalError(c, "failed to list skills: "+err.Error())
	}

	return util.Success(c, skills)
}

func listSkillsInPod(ctx context.Context, botID string) ([]SkillInfo, error) {
	client := k8s.GetClient()
	namespace := k8s.GetNamespace()

	// Get pod name
	deploymentName := k8s.GetDeploymentName(botID)
	pods, err := client.CoreV1().Pods(namespace).List(ctx, k8s.ListOptions(deploymentName))
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return []SkillInfo{}, nil
	}

	podName := pods.Items[0].Name

	// Execute ls command in pod
	output, err := k8s.ExecInPod(ctx, namespace, podName, "openclaw", []string{"ls", "-1", "/app/.openclaw/workspace/skills"})
	if err != nil {
		// Directory might not exist yet
		return []SkillInfo{}, nil
	}

	var skills []SkillInfo
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		if line != "" {
			skills = append(skills, SkillInfo{Name: line})
		}
	}

	return skills, nil
}
