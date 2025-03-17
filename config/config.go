package config

import (
	"context"
	"time"

	"github.com/joomcode/errorx"
	"github.com/sovamorco/gommon/config"
)

const (
	configNameDev = "config.dev.yaml"
)

type Config struct {
	UseDevLogger bool          `mapstructure:"use_dev_logger"`
	Timeout      time.Duration `mapstructure:"timeout"`
	Listener     BotConfig     `mapstructure:"listener"`
	Speakers     []BotConfig   `mapstructure:"speakers"`
}

type BotConfig struct {
	GuildID   string `mapstructure:"guild_id"`
	ChannelID string `mapstructure:"channel_id"`
	Token     string `mapstructure:"token"`
}

func LoadConfig(ctx context.Context) (*Config, error) {
	var res Config

	err := config.LoadConfig(ctx, configNameDev, &res)
	if err != nil {
		return nil, errorx.Decorate(err, "load config")
	}

	return &res, nil
}
