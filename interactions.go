package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"log"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Interaction handler
// ---------------------------------------------------------------------------

// interactionUserID returns the Discord user ID from an interaction,
// handling both guild (Member) and DM (User) contexts.
func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// respondEphemeral sends an ephemeral (user-only visible) text response
// to a slash command interaction.
func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func deferInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, ephemeral bool) error {
	data := &discordgo.InteractionResponseData{}
	if ephemeral {
		data.Flags = discordgo.MessageFlagsEphemeral
	}
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: data,
	})
}

func editInteractionContent(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
		log.Printf("interaction edit: %v", err)
	}
}

func editInteractionEmbeds(s *discordgo.Session, i *discordgo.InteractionCreate, embeds []*discordgo.MessageEmbed) {
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Embeds: &embeds}); err != nil {
		log.Printf("interaction embed edit: %v", err)
	}
}

func stopPlayback(s *discordgo.Session, guildID string, gs *guildState) {
	controls := gs.clearNowPlayingControlsFor("")
	gs.resetPlaybackState()
	clearNowPlayingMessageComponents(s, controls)
	s.RLock()
	vc, ok := s.VoiceConnections[guildID]
	s.RUnlock()
	if ok && vc != nil {
		gs.intentionalLeave.Store(true)
		if err := disconnectVoiceConnection(vc); err != nil {
			log.Printf("stop disconnect: %v", err)
		}
	}
}

func setAutoplay(gs *guildState, requestedState *bool) bool {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	enabled := !gs.autoplayEnabled
	if requestedState != nil {
		enabled = *requestedState
	}
	gs.autoplayEnabled = enabled
	return enabled
}

func autoplayStatusMessage(enabled bool) string {
	if enabled {
		return "Autoplay enabled. When the queue ends, I will add a related track."
	}
	return "Autoplay disabled."
}

func skipPlayback(s *discordgo.Session, guildID, textChannelID string, gs *guildState) string {
	req, shouldAutoplay := gs.prepareSkip()
	if !shouldAutoplay {
		return "Skipped."
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	nextTrack, err := resolveAutoplayTrack(ctx, req.seed, req.playedVideoIDs)
	cancel()
	if err != nil {
		log.Printf("skip autoplay: %v", err)
		return "Skipped. Autoplay could not find another related track."
	}
	_ = enqueueTrack(s, guildID, textChannelID, req.voiceChannelID, nextTrack)
	return "Skipped. Queued another related track."
}

func enqueueDeferredAction(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState, action guildAction) {
	if gs.enqueueAction(action) {
		return
	}
	editInteractionContent(s, i, "This server has too many queued music actions. Try again in a moment.")
}

// interactionHandler dispatches both autocomplete and slash command events.
func interactionHandler(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {

	case discordgo.InteractionApplicationCommandAutocomplete:
		data := i.ApplicationCommandData()
		if data.Name != "play" {
			return
		}
		var query string
		for _, opt := range data.Options {
			if opt.Focused && opt.Name == "query" {
				query = opt.StringValue()
				break
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
		defer cancel()
		choices, err := youtubeAutocompleteChoices(ctx, query)
		if err != nil {
			log.Printf("autocomplete: %v", err)
			choices = []*discordgo.ApplicationCommandOptionChoice{}
		}
		if choices == nil {
			choices = []*discordgo.ApplicationCommandOptionChoice{}
		}
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{Choices: choices},
		})

	case discordgo.InteractionApplicationCommand:
		if i.GuildID == "" {
			_ = respondEphemeral(s, i, "Use this bot inside a server.")
			return
		}
		gs := getGuildState(i.GuildID)
		if i.ApplicationCommandData().Name == "play" {
			handleSlashPlay(s, i, gs)
			return
		}
		if err := deferInteraction(s, i, true); err != nil {
			log.Printf("slash defer: %v", err)
			return
		}
		enqueueDeferredAction(s, i, gs, func() {
			handleQueuedSlashCommand(s, i, gs)
		})

	case discordgo.InteractionMessageComponent:
		if i.GuildID == "" {
			_ = respondEphemeral(s, i, "Use this bot inside a server.")
			return
		}
		gs := getGuildState(i.GuildID)
		if err := deferInteraction(s, i, true); err != nil {
			log.Printf("component defer: %v", err)
			return
		}
		enqueueDeferredAction(s, i, gs, func() {
			handleNowPlayingButton(s, i, gs)
		})
	}
}

func handleQueuedSlashCommand(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	switch i.ApplicationCommandData().Name {
	case "skip":
		handleSlashSkip(s, i, gs)
	case "stop":
		stopPlayback(s, i.GuildID, gs)
		editInteractionContent(s, i, "Stopped and cleared the queue.")
	case "shuffle":
		handleSlashShuffle(s, i, gs)
	case "remove":
		handleSlashRemove(s, i, gs)
	case "move":
		handleSlashMove(s, i, gs)
	case "clear":
		handleSlashClear(s, i, gs)
	case "queue":
		handleSlashQueue(s, i, gs)
	case "autoplay":
		handleSlashAutoplay(s, i, gs)
	default:
		editInteractionContent(s, i, "Unknown command.")
	}
}

func handleNowPlayingButton(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	action, ok := gs.activeNowPlayingAction(i.MessageComponentData().CustomID)
	if !ok {
		editInteractionContent(s, i, "That Now Playing message is no longer active.")
		return
	}
	switch action {
	case nowPlayingButtonSkip:
		editInteractionContent(s, i, skipPlayback(s, i.GuildID, i.ChannelID, gs))
	case nowPlayingButtonStop:
		stopPlayback(s, i.GuildID, gs)
		editInteractionContent(s, i, "Stopped and cleared the queue.")
	case nowPlayingButtonQueue:
		editInteractionEmbeds(s, i, []*discordgo.MessageEmbed{buildQueueEmbed(gs, 1)})
	case nowPlayingButtonAutoplay:
		enabled := setAutoplay(gs, nil)
		editInteractionContent(s, i, autoplayStatusMessage(enabled))
	default:
		editInteractionContent(s, i, "That button is no longer available.")
	}
}

func handleSlashSkip(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	editInteractionContent(s, i, skipPlayback(s, i.GuildID, i.ChannelID, gs))
}

func handleSlashShuffle(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	gs.mu.Lock()
	n := len(gs.songQueue)
	if n > 1 {
		rngMu.Lock()
		rng.Shuffle(n, func(i, j int) {
			gs.songQueue[i], gs.songQueue[j] = gs.songQueue[j], gs.songQueue[i]
		})
		rngMu.Unlock()
	}
	gs.mu.Unlock()
	editInteractionContent(s, i, "Queue shuffled.")
}

func handleSlashRemove(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	index := 0
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "index" {
			index = int(opt.IntValue())
			break
		}
	}
	removed, total, ok := gs.removeQueuedTrack(index)
	if !ok {
		editInteractionContent(s, i, fmt.Sprintf("There is no queued track #%d. Queue size: %d.", index, total))
		return
	}
	editInteractionContent(s, i, fmt.Sprintf("Removed #%d: %s", index, formatQueueTrack(removed)))
}

func handleSlashMove(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	from, to := 0, 0
	for _, opt := range i.ApplicationCommandData().Options {
		switch opt.Name {
		case "from":
			from = int(opt.IntValue())
		case "to":
			to = int(opt.IntValue())
		}
	}
	moved, total, ok := gs.moveQueuedTrack(from, to)
	if !ok {
		editInteractionContent(s, i, fmt.Sprintf("Could not move #%d to #%d. Queue size: %d.", from, to, total))
		return
	}
	if from == to {
		editInteractionContent(s, i, fmt.Sprintf("#%d is already there: %s", from, formatQueueTrack(moved)))
		return
	}
	editInteractionContent(s, i, fmt.Sprintf("Moved #%d to #%d: %s", from, to, formatQueueTrack(moved)))
}

func handleSlashClear(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	n := gs.clearQueuedTracks()
	if n == 0 {
		editInteractionContent(s, i, "Queue is already empty.")
		return
	}
	editInteractionContent(s, i, fmt.Sprintf("Cleared %d queued %s.", n, pluralize(n, "track", "tracks")))
}

func handleSlashQueue(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	page := 1
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "page" {
			page = int(opt.IntValue())
			break
		}
	}
	editInteractionEmbeds(s, i, []*discordgo.MessageEmbed{buildQueueEmbed(gs, page)})
}

func handleSlashAutoplay(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	var requestedState *bool
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "enabled" {
			value := opt.BoolValue()
			requestedState = &value
			break
		}
	}

	enabled := setAutoplay(gs, requestedState)
	editInteractionContent(s, i, autoplayStatusMessage(enabled))
}

// handleSlashPlay handles the /play command: verifies the user is in a voice
// channel, defers the response immediately (to avoid Discord's 3-second
// timeout), then resolves and enqueues the track through the per-guild action lane.
func handleSlashPlay(s *discordgo.Session, i *discordgo.InteractionCreate, gs *guildState) {
	uid := interactionUserID(i)
	if uid == "" {
		_ = respondEphemeral(s, i, "Could not identify your user.")
		return
	}

	voiceChID, err := voiceChannelForUser(s, i.GuildID, uid)
	if err != nil {
		msg := "Join a voice channel first, then run `/play`."
		if !errors.Is(err, errNotInVoice) {
			msg = "Could not read voice state. Try again in a moment."
		}
		_ = respondEphemeral(s, i, msg)
		return
	}

	var query string
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "query" {
			query = strings.TrimSpace(opt.StringValue())
			break
		}
	}
	if query == "" {
		_ = respondEphemeral(s, i, "Enter a search term or paste a YouTube URL.")
		return
	}

	if err := deferInteraction(s, i, false); err != nil {
		log.Printf("slash defer: %v", err)
		return
	}

	ch, guildID := i.ChannelID, i.GuildID
	enqueueDeferredAction(s, i, gs, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		msg := runPlayFlow(ctx, s, guildID, ch, voiceChID, query)
		if msg == "" {
			msg = "Queued — starting playback in your voice channel."
		}
		editInteractionContent(s, i, msg)
	})
}
