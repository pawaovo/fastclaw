package runtime

import (
	"context"
	"errors"
	"net"
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
	return model.AcquireEndpointLease(botID, endpoints)
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
