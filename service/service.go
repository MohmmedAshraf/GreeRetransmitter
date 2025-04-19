package service

import (
	"context"
	"slices"
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
	stopTimeout     = 5 * time.Second

	errorTimeThreshold        = 10 * time.Second
	errorCountThresholdPerBot = 250
)

type HandlerFunc func(ctx context.Context, sess *discordgo.Session, i *discordgo.InteractionCreate) error

type Service struct {
	errChan      chan error
	runningGuild string
	stopChan     chan struct{}
	shuttingDown bool
	cfg          *config.Config

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
	if b.VC != nil {
		err := b.VC.Disconnect()
		if err != nil {
			return errorx.Decorate(err, "disconnect from voice channel")
		}
	}

	err := b.Session.Close()
	if err != nil {
		return errorx.Decorate(err, "close session")
	}

	return nil
}

func New(ctx context.Context, cfg *config.Config, errChan chan error) (*Service, error) {
	s := &Service{
		errChan:      errChan,
		runningGuild: "",
		stopChan:     make(chan struct{}),
		shuttingDown: false,
		cfg:          cfg,
		listener:     nil,
		speakers:     nil,
	}

	err := s.initListener(ctx)
	if err != nil {
		return nil, errorx.Decorate(err, "init listener")
	}

	return s, nil
}

func (s *Service) Shutdown(logger *zerolog.Logger) {
	err := s.stop()
	if err != nil {
		logger.Error().Err(err).Msg("Failed to stop bots")
	}

	err = s.listener.Close()
	if err != nil {
		logger.Error().Err(err).Msg("Failed to close listener")
	}
}

func (s *Service) initListener(ctx context.Context) error {
	handlers := map[string]HandlerFunc{
		"ping":  s.handlePing,
		"start": s.handleStart,
		"stop":  s.handleStop,
	}

	listener, err := s.initSession(zerolog.Ctx(ctx), s.cfg.Listener.Token)
	if err != nil {
		return errorx.Decorate(err, "init listener")
	}

	listener.Session.AddHandler(s.handleCommand(ctx, handlers))

	for guild := range s.cfg.Guilds {
		for command := range handlers {
			_, err = listener.Session.ApplicationCommandCreate(
				listener.Session.State.User.ID,
				guild,
				//nolint:exhaustruct
				&discordgo.ApplicationCommand{
					Name:        command,
					Description: command,
				},
			)
			if err != nil {
				return errorx.Decorate(err, "create application command %s in guild %s", command, guild)
			}
		}
	}

	s.listener = listener

	return nil
}

func (s *Service) handleCommand(
	ctx context.Context, handlers map[string]HandlerFunc,
) func(sess *discordgo.Session, i *discordgo.InteractionCreate) {
	return func(sess *discordgo.Session, i *discordgo.InteractionCreate) {
		handler, ok := handlers[i.ApplicationCommandData().Name]
		if !ok {
			return
		}

		err := handler(ctx, sess, i)
		if err != nil {
			logger := zerolog.Ctx(ctx)

			logger.Error().Err(err).Str("command", i.ApplicationCommandData().Name).Msg("Failed to handle command")
		}
	}
}

func (s *Service) handlePing(_ context.Context, sess *discordgo.Session, i *discordgo.InteractionCreate) error {
	return s.respond(sess, i.Interaction, "pong")
}

func (s *Service) handleStart(ctx context.Context, sess *discordgo.Session, i *discordgo.InteractionCreate) error {
	err := s.ack(sess, i.Interaction)
	if err != nil {
		return errorx.Decorate(err, "ack interaction")
	}

	if _, ok := s.cfg.Guilds[i.GuildID]; !ok {
		return s.updateResponse(sess, i.Interaction, "This guild is not configured to use this bot")
	}

	if !s.checkPerms(i) {
		return s.updateResponse(sess, i.Interaction, "You do not have permission to start the bots")
	}

	if s.runningGuild != "" {
		resp := "The bots are already running"
		if s.runningGuild != i.GuildID {
			resp += " in another guild"
		}

		return s.updateResponse(sess, i.Interaction, resp)
	}

	logger := zerolog.Ctx(ctx)

	err = s.start(logger, i.GuildID)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to start the bots")

		return s.updateResponse(sess, i.Interaction,
			"Error starting the bots. Please check the logs or tell someone to check them!")
	}

	go s.run(ctx, i.GuildID)

	return s.updateResponse(sess, i.Interaction, "Started the bots")
}

func (s *Service) handleStop(ctx context.Context, sess *discordgo.Session, i *discordgo.InteractionCreate) error {
	err := s.ack(sess, i.Interaction)
	if err != nil {
		return errorx.Decorate(err, "ack interaction")
	}

	if _, ok := s.cfg.Guilds[i.GuildID]; !ok {
		return s.updateResponse(sess, i.Interaction, "This guild is not configured to use this bot")
	}

	if !s.checkPerms(i) {
		return s.updateResponse(sess, i.Interaction, "You do not have permission to stop the bots")
	}

	if s.runningGuild == "" {
		return s.updateResponse(sess, i.Interaction, "The bots are not running")
	}

	if s.runningGuild != i.GuildID {
		return s.updateResponse(sess, i.Interaction, "The bots are running in another guild. Go there to stop them")
	}

	logger := zerolog.Ctx(ctx)

	select {
	case <-ctx.Done():
		return nil
	case s.stopChan <- struct{}{}:
	case <-time.After(stopTimeout):
		logger.Error().Msg("Timed out while trying to stop bots")

		return s.updateResponse(sess, i.Interaction,
			"Error stopping the bots. Please check the logs or tell someone to check them!")
	}

	err = s.stop()
	if err != nil {
		logger.Error().Err(err).Msg("Failed to stop the bots")

		return s.updateResponse(sess, i.Interaction,
			"Error stopping the bots. Please check the logs or tell someone to check them!")
	}

	return s.updateResponse(sess, i.Interaction, "Stopped the bots")
}

func (s *Service) ack(sess *discordgo.Session, i *discordgo.Interaction) error {
	//nolint:exhaustruct
	err := sess.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		return errorx.Decorate(err, "ack interaction")
	}

	return nil
}

func (s *Service) respond(sess *discordgo.Session, i *discordgo.Interaction, msg string) error {
	//nolint:exhaustruct
	resp := discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}

	err := sess.InteractionRespond(i, &resp)
	if err != nil {
		return errorx.Decorate(err, "respond to command")
	}

	return nil
}

func (s *Service) updateResponse(sess *discordgo.Session, i *discordgo.Interaction, msg string) error {
	//nolint:exhaustruct
	resp := discordgo.WebhookEdit{
		Content: &msg,
	}

	_, err := sess.InteractionResponseEdit(i, &resp)
	if err != nil {
		return errorx.Decorate(err, "update response to command")
	}

	return nil
}

func (s *Service) checkPerms(i *discordgo.InteractionCreate) bool {
	for _, role := range i.Member.Roles {
		if slices.Contains(s.cfg.Guilds[i.GuildID].StarterRoles, role) {
			return true
		}
	}

	return false
}

func (s *Service) start(logger *zerolog.Logger, guildID string) error {
	var eg multierror.Group

	eg.Go(func() error {
		return s.listener.connectToVC(s, logger, guildID, s.cfg.Guilds[guildID].ListenerChannel, true, false)
	})

	speakerChannels := s.cfg.Guilds[guildID].SpeakerChannels

	s.speakers = make([]*Bot, len(speakerChannels))

	for i, channelID := range speakerChannels {
		token := s.cfg.Speakers[i].Token

		eg.Go(func() error {
			return s.initSpeaker(i, logger, token, guildID, channelID)
		})
	}

	err := eg.Wait().ErrorOrNil()
	if err != nil {
		return errorx.Decorate(err, "start bots")
	}

	return nil
}

func (s *Service) stop() error {
	var eg multierror.Group

	s.shuttingDown = true

	if s.listener.VC != nil {
		eg.Go(func() error {
			return s.listener.VC.Disconnect()
		})
	}

	for _, speaker := range s.speakers {
		eg.Go(func() error {
			return speaker.Close()
		})
	}

	err := eg.Wait().ErrorOrNil()
	if err != nil {
		return errorx.Decorate(err, "stop bots")
	}

	s.listener.VC = nil
	s.speakers = nil

	return nil
}

func (s *Service) initSpeaker(i int, logger *zerolog.Logger, token, guildID, channelID string) error {
	speaker, err := s.initSession(logger, token)
	if err != nil {
		return errorx.Decorate(err, "init speaker %d", i)
	}

	err = speaker.connectToVC(s, logger, guildID, channelID, false, true)
	if err != nil {
		return errorx.Decorate(err, "connect speaker %d to voice channel", i)
	}

	s.speakers[i] = speaker

	return nil
}

func (s *Service) run(ctx context.Context, guildID string) {
	s.runningGuild = guildID
	s.shuttingDown = false

	errs := make([]time.Time, 0)

	var errsMu sync.Mutex

	errorCountThreshold := errorCountThresholdPerBot * len(s.speakers)

	for s.runningGuild != "" {
		s.listener.vcMu.RLock()

		vc := s.listener.VC

		s.listener.vcMu.RUnlock()

		var packet *discordgo.Packet

		select {
		case <-ctx.Done():
			return
		case packet = <-vc.OpusRecv:
		case <-s.stopChan:
			s.runningGuild = ""

			return
		case <-time.After(opusSendTimeout):
			continue
		}

		// logger.Trace().Uint16("seq", packet.Sequence).Msg("Got opus packet")
		for _, speaker := range s.speakers {
			go func() {
				if !s.sendPacketToSpeaker(ctx, speaker, packet) {
					errsMu.Lock()
					errs = s.handleTimeout(zerolog.Ctx(ctx), errs, errorCountThreshold)
					errsMu.Unlock()
				}
			}()
		}
	}
}

// returns whether it succeeded.
func (s *Service) sendPacketToSpeaker(ctx context.Context, speaker *Bot, packet *discordgo.Packet) bool {
	speaker.vcMu.RLock()

	vc := speaker.VC

	speaker.vcMu.RUnlock()

	select {
	case <-ctx.Done():
		return true
	case vc.OpusSend <- packet.Opus:
		return true
	case <-time.After(opusSendTimeout):
		return false
	}
}

func (s *Service) handleTimeout(logger *zerolog.Logger, errs []time.Time, errorCountThreshold int) []time.Time {
	errs = combErrors(errs)
	errs = append(errs, time.Now())

	logger.Error().Int("errs", len(errs)).Msg("Timeout while trying to send audio packet to speaker")

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

func (s *Service) initSession(logger *zerolog.Logger, token string) (*Bot, error) {
	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, errorx.Decorate(err, "create new session")
	}

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

	return b, nil
}

func (b *Bot) connectToVC(s *Service, logger *zerolog.Logger, guildID, channelID string, mute, deaf bool) error {
	b.Session.AddHandler(b.handleVoiceError(s, logger, guildID, channelID, mute, deaf))

	vc, err := b.Session.ChannelVoiceJoin(guildID, channelID, mute, deaf)
	if err != nil {
		return errorx.Decorate(err, "join voice channel")
	}

	vc.LogLevel = discordgo.LogInformational

	b.VC = vc

	return nil
}

func (b *Bot) handleVoiceError(
	s *Service, logger *zerolog.Logger, guildID, channelID string, mute, deaf bool,
) func(_ *discordgo.Session, ev *discordgo.VoiceStateUpdate) {
	return func(_ *discordgo.Session, ev *discordgo.VoiceStateUpdate) {
		if ev.Member.User.ID != b.Session.State.User.ID ||
			ev.BeforeUpdate == nil ||
			ev.BeforeUpdate.ChannelID != channelID ||
			s.shuttingDown ||
			b.reconnecting ||
			b.VC == nil {
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

		vc, err := b.Session.ChannelVoiceJoin(guildID, channelID, mute, deaf)
		if err != nil {
			logger.Error().Err(err).Msg("Failed to rejoin voice channel")

			s.errChan <- errorx.Decorate(err, "connect to voice channel")
		}

		b.VC = vc
	}
}
