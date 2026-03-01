package runtime

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/spf13/viper"
)

var (
	dockerPoolGuardOnce sync.Once
	// unhealthyCounts stores consecutive health-check failure counts by bot ID.
	unhealthyCounts sync.Map
)

func startDockerPoolGuard() {
	interval := time.Duration(viper.GetInt("docker_pool.health_check_interval_seconds")) * time.Second
	if interval <= 0 {
		interval = 20 * time.Second
	}

	dockerPoolGuardOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for range ticker.C {
				reconcileDockerPool(context.Background())
			}
		}()
		log.Printf("docker_pool guard started (interval=%s)", interval)
	})
}

func reconcileDockerPool(ctx context.Context) {
	bots, err := model.ListBotsByStatus(model.BotStatusRunning)
	if err != nil {
		log.Printf("docker_pool guard: list running bots failed: %v", err)
		return
	}

	configured := getPoolEndpoints()
	if len(configured) == 0 {
		return
	}
	healthy := healthyEndpoints(configured)
	failThreshold := viper.GetInt("docker_pool.health_fail_threshold")
	if failThreshold <= 0 {
		failThreshold = 3
	}

	for _, bot := range bots {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ep := strings.TrimSpace(bot.Endpoint)
		if ep == "" || !containsEndpoint(configured, ep) || !checkEndpointReady(ep) {
			failures := incrementUnhealthy(bot.ID)
			if failures < failThreshold {
				continue
			}
			if err := recoverRunningBot(bot, healthy); err != nil {
				log.Printf("docker_pool guard: recover bot %s failed: %v", bot.ID, err)
			}
			unhealthyCounts.Delete(bot.ID)
			continue
		}

		unhealthyCounts.Delete(bot.ID)
	}
}

func recoverRunningBot(bot *model.Bot, healthy []string) error {
	// Always release the current lease first so this bot can be re-assigned.
	_ = model.ReleaseEndpointLease(bot.ID)

	if len(healthy) == 0 {
		if err := model.UpdateBotStatus(bot.ID, model.BotStatusError, ""); err != nil {
			return err
		}
		return nil
	}

	endpoint, err := model.AcquireEndpointLease(bot.ID, healthy)
	if err != nil {
		// Keep status explicit so portal can surface this to users.
		_ = model.UpdateBotStatus(bot.ID, model.BotStatusError, "")
		return err
	}

	return model.UpdateBotStatus(bot.ID, model.BotStatusRunning, endpoint)
}

func healthyEndpoints(configured []string) []string {
	out := make([]string, 0, len(configured))
	for _, ep := range configured {
		if checkEndpointReady(ep) {
			out = append(out, ep)
		}
	}
	return out
}

func containsEndpoint(endpoints []string, endpoint string) bool {
	for _, ep := range endpoints {
		if ep == endpoint {
			return true
		}
	}
	return false
}

func incrementUnhealthy(botID string) int {
	raw, _ := unhealthyCounts.LoadOrStore(botID, 0)
	current, _ := raw.(int)
	current++
	unhealthyCounts.Store(botID, current)
	return current
}
