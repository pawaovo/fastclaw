package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/fastclaw-ai/fastclaw/model"
)

// SyncBotConfigSections applies selected OpenClaw config sections from DB to a docker_pool instance.
// It merges the sections into /home/node/.openclaw/openclaw.json inside the allocated container.
func SyncBotConfigSections(ctx context.Context, bot *model.Bot, sections ...string) error {
	if bot == nil {
		return fmt.Errorf("bot is nil")
	}
	if !IsDockerPoolMode() {
		return nil
	}
	endpoint := strings.TrimSpace(bot.Endpoint)
	if endpoint == "" {
		return fmt.Errorf("bot endpoint is empty")
	}

	container, err := resolvePoolContainer(endpoint)
	if err != nil {
		return err
	}

	cfg, err := bot.GetOpenClawConfig()
	if err != nil {
		return fmt.Errorf("failed to load bot config: %w", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal bot config: %w", err)
	}
	var cfgMap map[string]interface{}
	if err := json.Unmarshal(raw, &cfgMap); err != nil {
		return fmt.Errorf("failed to normalize bot config: %w", err)
	}

	patch := make(map[string]interface{})
	if len(sections) == 0 {
		patch = cfgMap
	} else {
		for _, section := range sections {
			section = strings.TrimSpace(section)
			if section == "" {
				continue
			}
			if v, ok := cfgMap[section]; ok {
				patch[section] = v
			}
		}
	}
	if len(patch) == 0 {
		return nil
	}

	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal config patch: %w", err)
	}
	patchB64 := base64.StdEncoding.EncodeToString(patchJSON)
	nodeScript := fmt.Sprintf(
		`const fs=require("fs");`+
			`const p="/home/node/.openclaw/openclaw.json";`+
			`let c={};try{c=JSON.parse(fs.readFileSync(p,"utf8"))}catch(e){}`+
			`Object.assign(c,JSON.parse(Buffer.from("%s","base64").toString()));`+
			`fs.writeFileSync(p,JSON.stringify(c,null,2)+"\n")`,
		patchB64,
	)

	cmd := exec.CommandContext(ctx, "docker", "exec", container, "node", "-e", nodeScript)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to sync config to container %s: %w: %s", container, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func resolvePoolContainer(endpoint string) (string, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint %q: %w", endpoint, err)
	}
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	if host != "" && host != "127.0.0.1" && host != "localhost" {
		return host, nil
	}

	cmd := exec.Command("docker", "ps", "--format", "{{.Names}}\t{{.Ports}}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to list docker containers: %w: %s", err, strings.TrimSpace(string(output)))
	}
	lines := strings.Split(string(output), "\n")
	match := ":" + port + "->"
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		ports := strings.TrimSpace(parts[1])
		if name == "" || ports == "" {
			continue
		}
		if strings.Contains(ports, match) {
			return name, nil
		}
	}

	return "", fmt.Errorf("unable to resolve docker container for endpoint %s", endpoint)
}
