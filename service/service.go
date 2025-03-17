package service

import (
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/hashicorp/go-multierror"
	"github.com/rs/zerolog"
	"github.com/sovamorco/errorx"
	"github.com/sovamorco/gree-retransmitter/config"
)

const (
	opusSendTimeout = 500 * time.Millisecond

	errorTimeThreshold        = 10 * time.Second
	errorCountThresholdPerBot = 250
)

type Service struct {
	errChan      chan error
	shuttingDown bool

	listener *Bot
	speakers []*Bot
}

type Bot struct {
	Session      *discordgo.Session
	VC           *discordgo.VoiceConnection
	vcMu         sync.RWMutex `exhaustruct:"optional"`
	reconnecting bool
}

func (b *Bot) Close() error {
	err := b.VC.Disconnect()
	if err != nil {
		return errorx.Decorate(err, "disconnect from voice channel")
	}

	err = b.Session.Close()
	if err != nil {
		return errorx.Decorate(err, "close session")
	}

	return nil
}

func New(logger *zerolog.Logger, cfg *config.Config, errChan chan error) (*Service, error) {
	s := &Service{
		errChan:      errChan,
		shuttingDown: false,
		listener:     nil,
		speakers:     make([]*Bot, len(cfg.Speakers)),
	}

	var eg multierror.Group

	eg.Go(func() error {
		return s.initListener(logger, cfg.Listener)
	})

	for i, speakerCfg := range cfg.Speakers {
		eg.Go(func() error {
			return s.initSpeaker(i, logger, speakerCfg)
		})
	}

	err := eg.Wait().ErrorOrNil()
	if err != nil {
		return nil, errorx.Decorate(err, "init bots")
	}

	return s, nil
}

func (s *Service) initListener(logger *zerolog.Logger, cfg config.BotConfig) error {
	listener, err := s.initSession(logger, cfg, true, false)
	if err != nil {
		return errorx.Decorate(err, "init listener")
	}

	s.listener = listener

	return nil
}

func (s *Service) initSpeaker(i int, logger *zerolog.Logger, cfg config.BotConfig) error {
	speaker, err := s.initSession(logger, cfg, false, true)
	if err != nil {
		return errorx.Decorate(err, "init speaker %d", i)
	}

	s.speakers[i] = speaker

	return nil
}

func (s *Service) Run(logger *zerolog.Logger) {
	errs := make([]time.Time, 0)

	var errsMu sync.Mutex

	errorCountThreshold := errorCountThresholdPerBot * len(s.speakers)

	for {
		s.listener.vcMu.RLock()

		vc := s.listener.VC

		s.listener.vcMu.RUnlock()

		var packet *discordgo.Packet

		select {
		case packet = <-vc.OpusRecv:
		case <-time.After(opusSendTimeout):
			continue
		}

		// logger.Trace().Uint16("seq", packet.Sequence).Msg("Got opus packet")
		for _, speaker := range s.speakers {
			go func() {
				speaker.vcMu.RLock()

				vc := speaker.VC

				speaker.vcMu.RUnlock()

				select {
				case vc.OpusSend <- packet.Opus:
				case <-time.After(opusSendTimeout):
					errsMu.Lock()
					errs = s.handleTimeout(logger, errs, errorCountThreshold)
					errsMu.Unlock()
				}
			}()
		}
	}
}

func (s *Service) handleTimeout(logger *zerolog.Logger, errs []time.Time, errorCountThreshold int) []time.Time {
	errs = combErrors(errs)
	errs = append(errs, time.Now())

	logger.Error().Int("errs", len(errs)).Msg("Timeout while trying to receive audio packet from listener")

	if len(errs) >= errorCountThreshold {
		s.errChan <- errorx.InternalError.New("too many errors while running voice loop")
	}

	return errs
}

func combErrors(errs []time.Time) []time.Time {
	cutoff := time.Now().Add(-errorTimeThreshold)

	for i, errTime := range errs {
		if errTime.After(cutoff) {
			errs = errs[i:]

			break
		}
	}

	return errs
}

func (s *Service) Shutdown(logger *zerolog.Logger) {
	var eg multierror.Group

	s.shuttingDown = true

	for _, speaker := range s.speakers {
		eg.Go(func() error {
			err := speaker.Close()
			if err != nil {
				return errorx.Decorate(err, "close speaker")
			}

			return nil
		})
	}

	eg.Go(func() error {
		err := s.listener.Close()
		if err != nil {
			return errorx.Decorate(err, "close listener")
		}

		return nil
	})

	err := eg.Wait().ErrorOrNil()
	if err != nil {
		logger.Error().Err(err).Msg("Failed to shutdown service")
	}
}

func (s *Service) initSession(logger *zerolog.Logger, cfg config.BotConfig, mute, deaf bool) (*Bot, error) {
	sess, err := discordgo.New(cfg.Token)
	if err != nil {
		return nil, errorx.Decorate(err, "create new session")
	}

	sess.Identify.Intents = discordgo.IntentGuildVoiceStates

	err = sess.Open()
	if err != nil {
		return nil, errorx.Decorate(err, "open session")
	}

	logger.Info().Any("application", sess.State.Application).Any("user", sess.State.User).Msg("Connected")

	b := &Bot{
		Session:      sess,
		VC:           nil,
		reconnecting: false,
	}

	sess.AddHandler(b.handleVoiceError(s, logger, cfg, mute, deaf))

	vc, err := sess.ChannelVoiceJoin(cfg.GuildID, cfg.ChannelID, mute, deaf)
	if err != nil {
		return nil, errorx.Decorate(err, "join voice channel")
	}

	vc.LogLevel = discordgo.LogInformational

	b.VC = vc

	return b, nil
}

func (b *Bot) handleVoiceError(
	s *Service, logger *zerolog.Logger, cfg config.BotConfig, mute, deaf bool,
) func(_ *discordgo.Session, ev *discordgo.VoiceStateUpdate) {
	return func(_ *discordgo.Session, ev *discordgo.VoiceStateUpdate) {
		if ev.Member.User.ID != b.Session.State.User.ID ||
			ev.BeforeUpdate == nil ||
			ev.BeforeUpdate.ChannelID != cfg.ChannelID ||
			s.shuttingDown ||
			b.reconnecting {
			return
		}

		logger.Info().Any("event", ev).Msg("Voice state update")

		if ev.ChannelID != "" {
			return
		}

		logger.Info().Msg("Disconnected from voice, trying to reconnect")

		b.vcMu.Lock()
		defer b.vcMu.Unlock()

		b.reconnecting = true
		defer func() { b.reconnecting = false }()

		err := b.VC.Disconnect()
		if err != nil {
			logger.Error().Err(err).Msg("Failed to disconnect from disconnected vc")
		}

		vc, err := b.Session.ChannelVoiceJoin(cfg.GuildID, cfg.ChannelID, mute, deaf)
		if err != nil {
			logger.Error().Err(err).Msg("Failed to rejoin voice channel")

			s.errChan <- errorx.Decorate(err, "connect to voice channel")
		}

		b.VC = vc
	}
}
