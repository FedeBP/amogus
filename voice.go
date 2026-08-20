package main

import (
	"github.com/bwmarrin/discordgo"
	"log"
)

func disconnectVoiceConnection(vc *discordgo.VoiceConnection) error {
	if vc == nil {
		return nil
	}
	// discordgo may attempt to reconnect a VoiceConnection using its last
	// ChannelID after transport errors. Clear it before closing local voice
	// resources so an external kick or stop cannot auto-rejoin later.
	vc.Lock()
	vc.ChannelID = ""
	vc.Unlock()
	return vc.Disconnect()
}

func closeLocalVoiceConnection(s *discordgo.Session, guildID string, vc *discordgo.VoiceConnection) {
	if vc == nil {
		return
	}
	vc.Lock()
	vc.ChannelID = ""
	vc.Unlock()
	vc.Close()
	if s != nil {
		s.Lock()
		delete(s.VoiceConnections, guildID)
		s.Unlock()
	}
}

// ---------------------------------------------------------------------------
// Voice helpers
// ---------------------------------------------------------------------------

// voiceChannelForUser returns the voice channel ID that userID is currently
// in within guildID, or errNotInVoice if they are not in any channel.
func voiceChannelForUser(s *discordgo.Session, guildID, userID string) (string, error) {
	g, err := s.State.Guild(guildID)
	if err != nil {
		return "", err
	}
	for _, vs := range g.VoiceStates {
		if vs.UserID == userID && vs.ChannelID != "" {
			return vs.ChannelID, nil
		}
	}
	return "", errNotInVoice
}

// voiceStateHandler clears playback when the bot leaves voice or when its
// current voice channel becomes empty.
func voiceStateHandler(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
	if vs == nil || vs.VoiceState == nil || BotID == "" {
		return
	}
	if vs.UserID != BotID {
		maybeDisconnectEmptyVoiceChannel(s, vs.GuildID)
		return
	}
	if vs.ChannelID != "" {
		return // bot joined or moved — not a leave event
	}

	gs := getGuildState(vs.GuildID)
	wasIntentional := gs.intentionalLeave.CompareAndSwap(true, false)
	previousChannelID := ""
	if vs.BeforeUpdate != nil {
		previousChannelID = vs.BeforeUpdate.ChannelID
	}

	if wasIntentional {
		// Normal stop/idle disconnect path.
	} else if recovery, ok := gs.prepareUnexpectedVoiceLeaveRecovery(vs.GuildID); ok {
		log.Printf("Bot voice connection dropped in guild %s; reconnecting playback. previous_voice=%s", vs.GuildID, previousChannelID)
		gs.killPlaybackProcesses()
		clearNowPlayingMessageComponents(s, recovery.controls)
		_, _ = s.ChannelMessageSend(recovery.textChannelID, "Voice connection dropped; reconnecting and restarting the current track.")

		s.RLock()
		vc, ok := s.VoiceConnections[vs.GuildID]
		s.RUnlock()
		if ok && vc != nil {
			closeLocalVoiceConnection(s, vs.GuildID, vc)
		}
		safeGo("playback recovery", func() {
			playNextSong(s, vs.GuildID, recovery.textChannelID, recovery.generation)
		})
		return
	} else {
		log.Printf("Bot was removed from voice in guild %s; clearing playback state. previous_voice=%s", vs.GuildID, previousChannelID)
	}

	gs.resetPlaybackState()

	s.RLock()
	vc, ok := s.VoiceConnections[vs.GuildID]
	s.RUnlock()
	if ok && vc != nil {
		closeLocalVoiceConnection(s, vs.GuildID, vc)
	}
}

func maybeDisconnectEmptyVoiceChannel(s *discordgo.Session, guildID string) {
	channelID := botVoiceChannelID(s, guildID)
	if channelID == "" || !voiceChannelIsEmpty(s, guildID, channelID) {
		return
	}

	gs := getGuildState(guildID)
	if !gs.enqueueAction(func() {
		channelID := botVoiceChannelID(s, guildID)
		if channelID == "" || !voiceChannelIsEmpty(s, guildID, channelID) {
			return
		}
		disconnectEmptyVoiceChannel(s, guildID, gs, channelID)
	}) {
		log.Printf("empty voice disconnect skipped in guild %s: action queue is full", guildID)
	}
}

func botVoiceChannelID(s *discordgo.Session, guildID string) string {
	if s == nil {
		return ""
	}
	s.RLock()
	vc := s.VoiceConnections[guildID]
	s.RUnlock()
	if vc != nil {
		vc.RLock()
		channelID := vc.ChannelID
		vc.RUnlock()
		if channelID != "" {
			return channelID
		}
	}

	if s.State == nil {
		return ""
	}
	g, err := s.State.Guild(guildID)
	if err != nil {
		return ""
	}
	for _, voiceState := range g.VoiceStates {
		if voiceState.UserID == BotID {
			return voiceState.ChannelID
		}
	}
	return ""
}

func voiceChannelIsEmpty(s *discordgo.Session, guildID, channelID string) bool {
	if s == nil || s.State == nil {
		return false
	}
	g, err := s.State.Guild(guildID)
	if err != nil {
		log.Printf("empty voice check: %v", err)
		return false
	}
	return !voiceChannelHasListeners(g, channelID)
}

func voiceChannelHasListeners(g *discordgo.Guild, channelID string) bool {
	if g == nil || channelID == "" {
		return false
	}
	for _, voiceState := range g.VoiceStates {
		if voiceState.ChannelID == channelID && voiceState.UserID != BotID {
			return true
		}
	}
	return false
}

func disconnectEmptyVoiceChannel(s *discordgo.Session, guildID string, gs *guildState, channelID string) {
	log.Printf("Voice channel %s in guild %s is empty; stopping playback.", channelID, guildID)
	controls := gs.clearNowPlayingControlsFor("")
	gs.resetPlaybackState()
	clearNowPlayingMessageComponents(s, controls)

	s.RLock()
	vc := s.VoiceConnections[guildID]
	s.RUnlock()
	if vc == nil {
		return
	}
	vc.RLock()
	currentChannelID := vc.ChannelID
	vc.RUnlock()
	if currentChannelID != "" && currentChannelID != channelID {
		return
	}
	gs.intentionalLeave.Store(true)
	if err := disconnectVoiceConnection(vc); err != nil {
		log.Printf("empty voice disconnect: %v", err)
	}
}
