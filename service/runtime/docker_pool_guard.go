package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/spf13/viper"
	"gorm.io/gorm"
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
		reconcileDockerPool(context.Background())
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
	reapZombieLeases()

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
	currentEndpoint := strings.TrimSpace(bot.Endpoint)
	configured := getPoolEndpoints()
	if currentEndpoint != "" && containsEndpoint(configured, currentEndpoint) {
		_ = restartPoolContainerForEndpoint(currentEndpoint, configured)
		time.Sleep(2 * time.Second)
		if checkEndpointReady(currentEndpoint) {
			if err := model.UpdateBotStatus(bot.ID, model.BotStatusRunning, currentEndpoint); err != nil {
				return err
			}
			markDockerPoolBotWarmup(bot.ID)
			return nil
		}
	}

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

func reapZombieLeases() {
	leases, err := model.ListActiveEndpointLeases()
	if err != nil {
		log.Printf("docker_pool guard: list active leases failed: %v", err)
		return
	}

	for _, lease := range leases {
		bot, err := model.GetBotByID(lease.BotID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			_ = model.ReleaseEndpointLease(lease.BotID)
			clearDockerPoolBotWarmup(lease.BotID)
			continue
		}
		if err != nil {
			continue
		}
		if bot.Status != model.BotStatusRunning {
			_ = model.ReleaseEndpointLease(lease.BotID)
			clearDockerPoolBotWarmup(lease.BotID)
			continue
		}
		if strings.TrimSpace(bot.Endpoint) == "" {
			_ = model.UpdateBotStatus(bot.ID, model.BotStatusRunning, lease.Endpoint)
		}
	}
}

func restartPoolContainerForEndpoint(endpoint string, endpoints []string) error {
	name, err := resolvePoolContainerName(endpoint, endpoints)
	if err != nil {
		return err
	}

	timeout := time.Duration(viper.GetInt("docker_pool.container_restart_timeout_seconds")) * time.Second
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	output, err := exec.CommandContext(ctx, "docker", "restart", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart container %s failed: %w; output: %s", name, err, strings.TrimSpace(string(output)))
	}
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
