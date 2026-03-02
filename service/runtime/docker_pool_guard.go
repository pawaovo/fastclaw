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
	// warmupUntil stores a guard grace window for freshly started/recovered bots.
	warmupUntil sync.Map
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
		if isInWarmup(bot.ID) {
			unhealthyCounts.Delete(bot.ID)
			continue
		}
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

	if err := model.UpdateBotStatus(bot.ID, model.BotStatusRunning, endpoint); err != nil {
		return err
	}
	markDockerPoolBotWarmup(bot.ID)
	return nil
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

func markDockerPoolBotWarmup(botID string) {
	grace := time.Duration(viper.GetInt("docker_pool.health_startup_grace_seconds")) * time.Second
	if grace <= 0 {
		grace = 120 * time.Second
	}
	warmupUntil.Store(botID, time.Now().Add(grace))
}

func clearDockerPoolBotWarmup(botID string) {
	warmupUntil.Delete(botID)
	unhealthyCounts.Delete(botID)
}

func isInWarmup(botID string) bool {
	raw, ok := warmupUntil.Load(botID)
	if !ok {
		return false
	}
	until, ok := raw.(time.Time)
	if !ok {
		warmupUntil.Delete(botID)
		return false
	}
	if time.Now().After(until) {
		warmupUntil.Delete(botID)
		return false
	}
	return true
}
