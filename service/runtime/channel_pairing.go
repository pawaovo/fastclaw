package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
)

// ApproveChannelPairing approves a channel pairing code on the target runtime.
func ApproveChannelPairing(ctx context.Context, bot *model.Bot, channel, code string) (string, error) {
	if bot == nil {
		return "", fmt.Errorf("bot is nil")
	}
	channel = strings.TrimSpace(strings.ToLower(channel))
	code = strings.TrimSpace(code)
	if channel == "" {
		return "", fmt.Errorf("channel is required")
	}
	if code == "" {
		return "", fmt.Errorf("pairing code is required")
	}

	if !IsDockerPoolMode() {
		return k8s.ApproveChannelPairing(ctx, bot.ID, channel, code)
	}

	endpoint := strings.TrimSpace(bot.Endpoint)
	if endpoint == "" {
		return "", fmt.Errorf("bot endpoint is empty")
	}
	container, err := resolvePoolContainer(endpoint)
	if err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, "docker", "exec", container, "node", "/app/openclaw.mjs", "pairing", "approve", channel, code)
	output, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(output))
	if err != nil {
		if trimmed == "" {
			return "", fmt.Errorf("failed to approve pairing in %s: %w", container, err)
		}
		return "", fmt.Errorf("failed to approve pairing in %s: %w: %s", container, err, trimmed)
	}
	return trimmed, nil
}
