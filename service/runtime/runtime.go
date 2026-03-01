package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/spf13/viper"
)

const (
	ModeK8s        = "k8s"
	ModeDockerPool = "docker_pool"
)

func Mode() string {
	mode := strings.TrimSpace(strings.ToLower(viper.GetString("runtime.mode")))
	if mode == "" {
		return ModeK8s
	}
	if mode != ModeDockerPool {
		return ModeK8s
	}
	return mode
}

func IsDockerPoolMode() bool {
	return Mode() == ModeDockerPool
}

func Init() error {
	if IsDockerPoolMode() {
		if len(getPoolEndpoints()) == 0 {
			return errors.New("runtime.mode=docker_pool but docker_pool.endpoints is empty")
		}
		startDockerPoolGuard()
		return nil
	}
	return k8s.InitClient()
}

func StartBot(ctx context.Context, bot *model.Bot, k8sConfig *k8s.BotConfig) (string, error) {
	if IsDockerPoolMode() {
		if err := ensureRunningLimit(); err != nil {
			return "", err
		}
		return allocatePoolEndpoint(bot.ID)
	}

	if err := k8s.CreateDeployment(ctx, bot.ID, bot.UserID, bot.AccessToken, k8sConfig); err != nil {
		return "", err
	}
	endpoint, err := k8s.CreateService(ctx, bot.ID, bot.UserID)
	if err != nil {
		_ = k8s.DeleteDeployment(ctx, bot.ID)
		return "", err
	}
	return endpoint, nil
}

func StopBot(ctx context.Context, bot *model.Bot) error {
	if IsDockerPoolMode() {
		return model.ReleaseEndpointLease(bot.ID)
	}
	if err := k8s.DeleteDeployment(ctx, bot.ID); err != nil {
		return err
	}
	if err := k8s.DeleteService(ctx, bot.ID); err != nil {
		return err
	}
	return nil
}

func GetBotReady(ctx context.Context, bot *model.Bot) (bool, error) {
	if IsDockerPoolMode() {
		return checkEndpointReady(bot.Endpoint), nil
	}
	return k8s.GetDeploymentStatus(ctx, bot.ID)
}

func GetBotEndpoint(ctx context.Context, bot *model.Bot) (string, error) {
	if bot.Endpoint != "" {
		return bot.Endpoint, nil
	}
	if IsDockerPoolMode() {
		return "", errors.New("bot endpoint is empty")
	}
	return k8s.GetServiceEndpoint(ctx, bot.ID)
}

func DeploymentExists(ctx context.Context, botID string) (bool, error) {
	if IsDockerPoolMode() {
		return true, nil
	}
	return k8s.DeploymentExists(ctx, botID)
}

func getPoolEndpoints() []string {
	endpoints := viper.GetStringSlice("docker_pool.endpoints")
	out := make([]string, 0, len(endpoints))
	for _, ep := range endpoints {
		ep = strings.TrimSpace(ep)
		if ep != "" {
			out = append(out, ep)
		}
	}
	return out
}

func allocatePoolEndpoint(botID string) (string, error) {
	endpoints := getPoolEndpoints()
	if len(endpoints) == 0 {
		return "", errors.New("docker pool endpoint is empty")
	}
	endpoint, err := model.AcquireEndpointLease(botID, endpoints)
	if err != nil {
		return "", err
	}
	if err := resetPoolEndpoint(endpoint, endpoints); err != nil {
		_ = model.ReleaseEndpointLease(botID)
		return "", err
	}
	return endpoint, nil
}

func ReleaseBot(botID string) error {
	if !IsDockerPoolMode() {
		return nil
	}
	return model.ReleaseEndpointLease(botID)
}

func checkEndpointReady(endpoint string) bool {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", endpoint, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func ensureRunningLimit() error {
	maxRunning := viper.GetInt("runtime.max_running_bots")
	if maxRunning <= 0 {
		return nil
	}
	bots, err := model.ListBotsByStatus(model.BotStatusRunning)
	if err != nil {
		return err
	}
	if len(bots) >= maxRunning {
		return errors.New("max running bots limit reached")
	}
	return nil
}

func resetPoolEndpoint(endpoint string, endpoints []string) error {
	if viper.IsSet("docker_pool.reset_on_allocate") && !viper.GetBool("docker_pool.reset_on_allocate") {
		return nil
	}

	containerName, err := resolvePoolContainerName(endpoint, endpoints)
	if err != nil {
		return err
	}

	timeout := viper.GetDuration("docker_pool.reset_timeout")
	if timeout <= 0 {
		timeout = 40 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cleanupScript := strings.Join([]string{
		"set -eu",
		"rm -rf /home/node/.openclaw/agents/*",
		"rm -rf /home/node/.openclaw/workspace/*",
		"rm -rf /home/node/.openclaw/canvas/*",
		"rm -rf /home/node/.openclaw/cron/*",
		"if [ -f /home/node/.openclaw/openclaw.json ]; then sed -i -E 's/(\"apiKey\"[[:space:]]*:[[:space:]]*\")[^\"]*(\")/\\1\\2/g' /home/node/.openclaw/openclaw.json || true; fi",
		"if [ -f /home/node/.openclaw/.env ]; then sed -i '/^CUSTOM_API_KEY=/d' /home/node/.openclaw/.env || true; fi",
	}, "; ")

	cleanupOutput, cleanupErr := exec.CommandContext(ctx, "docker", "exec", containerName, "sh", "-lc", cleanupScript).CombinedOutput()
	if cleanupErr != nil {
		return fmt.Errorf("reset docker pool container %s failed: %w; output: %s", containerName, cleanupErr, strings.TrimSpace(string(cleanupOutput)))
	}

	restartOutput, restartErr := exec.CommandContext(ctx, "docker", "restart", containerName).CombinedOutput()
	if restartErr != nil {
		return fmt.Errorf("restart docker pool container %s failed: %w; output: %s", containerName, restartErr, strings.TrimSpace(string(restartOutput)))
	}

	return nil
}

func resolvePoolContainerName(endpoint string, endpoints []string) (string, error) {
	containerNames := viper.GetStringSlice("docker_pool.container_names")
	for idx, ep := range endpoints {
		if ep != endpoint {
			continue
		}
		if idx < len(containerNames) {
			name := strings.TrimSpace(containerNames[idx])
			if name != "" {
				return name, nil
			}
		}
		return fmt.Sprintf("openclaw-pool-%02d", idx+1), nil
	}
	return "", fmt.Errorf("endpoint %s not found in docker_pool.endpoints", endpoint)
}
