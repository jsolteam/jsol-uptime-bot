package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Node struct {
		ID       string `yaml:"id"`
		Priority int    `yaml:"priority"`
	} `yaml:"node"`
	Telegram struct {
		TokenEnv        string `yaml:"token_env"`
		DailyReportTime string `yaml:"daily_report_time"`
		Timezone        string `yaml:"timezone"`
	} `yaml:"telegram"`
	Storage struct {
		SQLitePath          string   `yaml:"sqlite_path"`
		BackupDir           string   `yaml:"backup_dir"`
		BackupInterval      Duration `yaml:"backup_interval"`
		BackupRetentionDays int      `yaml:"backup_retention_days"`
	} `yaml:"storage"`
	Coordination struct {
		RedisURLEnv string   `yaml:"redis_url_env"`
		Lease       Duration `yaml:"lease"`
		Heartbeat   Duration `yaml:"heartbeat"`
	} `yaml:"coordination"`
	Agent struct {
		Listen   string `yaml:"listen"`
		TokenEnv string `yaml:"token_env"`
	} `yaml:"agent"`
	Monitoring struct {
		Interval              Duration `yaml:"interval"`
		Timeout               Duration `yaml:"timeout"`
		FailureThreshold      int      `yaml:"failure_threshold"`
		RecoveryThreshold     int      `yaml:"recovery_threshold"`
		AgentFailureThreshold Duration `yaml:"agent_failure_threshold"`
		RetentionDays         int      `yaml:"retention_days"`
		InternetChecks        []string `yaml:"internet_checks"`
	} `yaml:"monitoring"`
	Servers []Server `yaml:"servers"`
	Admins  []int64  `yaml:"admins"`
}

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

type Server struct {
	ID       string `yaml:"id"`
	Name     string `yaml:"name"`
	Address  string `yaml:"address"`
	Priority int    `yaml:"priority"`
	AgentURL string `yaml:"agent_url"`
}

func Load(path string) (Config, error) {
	var c Config
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	if e = yaml.Unmarshal(b, &c); e != nil {
		return c, e
	}
	if c.Node.ID == "" || c.Storage.SQLitePath == "" || len(c.Servers) == 0 {
		return c, fmt.Errorf("обязательны node.id, storage.sqlite_path и servers")
	}
	if c.Node.Priority < 0 {
		return c, fmt.Errorf("node.priority не может быть отрицательным")
	}
	seen := make(map[string]struct{}, len(c.Servers))
	for _, server := range c.Servers {
		if server.ID == "" || server.Name == "" || server.Address == "" || server.AgentURL == "" {
			return c, fmt.Errorf("для каждого сервера обязательны id, name, address и agent_url")
		}
		if strings.ContainsAny(server.ID, ":*?[]") {
			return c, fmt.Errorf("недопустимый ID сервера %q", server.ID)
		}
		if _, ok := seen[server.ID]; ok {
			return c, fmt.Errorf("ID сервера повторяется: %s", server.ID)
		}
		seen[server.ID] = struct{}{}
		u, err := url.Parse(server.AgentURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return c, fmt.Errorf("некорректный agent_url для %s", server.ID)
		}
	}
	if len(c.Admins) == 0 {
		return c, fmt.Errorf("задайте хотя бы одного Telegram ID в admins")
	}
	if c.Telegram.TokenEnv == "" {
		c.Telegram.TokenEnv = "TELEGRAM_BOT_TOKEN"
	}
	if c.Agent.TokenEnv == "" {
		c.Agent.TokenEnv = "AGENT_TOKEN"
	}
	if c.Agent.Listen == "" {
		c.Agent.Listen = ":8080"
	}
	if c.Monitoring.Interval <= 0 {
		c.Monitoring.Interval = Duration(30 * time.Second)
	}
	if c.Monitoring.Timeout <= 0 {
		c.Monitoring.Timeout = Duration(5 * time.Second)
	}
	if c.Monitoring.FailureThreshold <= 0 {
		c.Monitoring.FailureThreshold = 3
	}
	if c.Monitoring.RecoveryThreshold <= 0 {
		c.Monitoring.RecoveryThreshold = 2
	}
	if c.Monitoring.RetentionDays <= 0 {
		c.Monitoring.RetentionDays = 30
	}
	if c.Storage.BackupDir == "" {
		c.Storage.BackupDir = c.Storage.SQLitePath + ".backups"
	}
	if c.Storage.BackupInterval <= 0 {
		c.Storage.BackupInterval = Duration(24 * time.Hour)
	}
	if c.Storage.BackupRetentionDays <= 0 {
		c.Storage.BackupRetentionDays = 14
	}
	if c.Coordination.Lease <= 0 {
		c.Coordination.Lease = Duration(15 * time.Second)
	}
	if c.Coordination.Heartbeat <= 0 {
		c.Coordination.Heartbeat = Duration(5 * time.Second)
	}
	if c.Telegram.Timezone == "" {
		c.Telegram.Timezone = "Europe/Moscow"
	}
	if _, err := time.LoadLocation(c.Telegram.Timezone); err != nil {
		return c, fmt.Errorf("неизвестный часовой пояс %q", c.Telegram.Timezone)
	}
	if c.Telegram.DailyReportTime != "" {
		if _, err := time.Parse("15:04", c.Telegram.DailyReportTime); err != nil {
			return c, fmt.Errorf("telegram.daily_report_time должен иметь формат ЧЧ:ММ")
		}
	}
	return c, nil
}
