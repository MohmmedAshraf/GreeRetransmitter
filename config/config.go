package config

import (
	"context"

	"github.com/sovamorco/errorx"
	"github.com/sovamorco/gommon/config"
)

const (
	configNameDev = "config.dev.yaml"
)

type Config struct {
	// If true - will pretty print logs, otherwise will json-print them.
	UseDevLogger bool                   `mapstructure:"use_dev_logger"`
	Listener     BotConfig              `mapstructure:"listener"`
	Speakers     []BotConfig            `mapstructure:"speakers"`
	Guilds       map[string]GuildConfig `mapstructure:"guilds"`
}

type BotConfig struct {
	Token string `mapstructure:"token"`
}

type GuildConfig struct {
	ListenerChannel string   `mapstructure:"listener_channel"`
	SpeakerChannels []string `mapstructure:"speaker_channels"`
	StarterRoles    []string `mapstructure:"starter_roles"`
}

func LoadConfig(ctx context.Context) (*Config, error) {
	var res Config

	err := config.LoadConfig(ctx, configNameDev, &res)
	if err != nil {
		return nil, errorx.Decorate(err, "load config")
	}

	for guild, gcfg := range res.Guilds {
		if len(gcfg.SpeakerChannels) > len(res.Speakers) {
			return nil, errorx.InitializationFailed.New("guild %s has more speaker channels than available speakers", guild)
		}
	}

	return &res, nil
}
