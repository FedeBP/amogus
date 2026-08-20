package main

import (
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Start initialises the Discord session, registers event handlers,
// and opens the WebSocket gateway connection.
func Start() {
	configureDiscordLogger()

	session, err := discordgo.New("Bot " + Token)
	if err != nil {
		log.Fatalf("Couldn't initialise bot: %v", err)
	}

	user, err := session.User("@me")
	if err != nil {
		log.Fatalf("Error getting bot user: %v", err)
	}
	BotID = user.ID

	session.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildVoiceStates

	session.AddHandler(func(s *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
		safeRun("voice state handler", func() {
			voiceStateHandler(s, vs)
		})
	})
	session.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		safeRun("interaction handler", func() {
			interactionHandler(s, i)
		})
	})
	session.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		safeRun("ready handler", func() {
			// Overwrite all slash commands on startup so any definition changes
			// take effect without manual intervention.
			appID := r.User.ID
			if r.Application != nil && r.Application.ID != "" {
				appID = r.Application.ID
			}
			if _, err := s.ApplicationCommandBulkOverwrite(appID, "", slashCommands); err != nil {
				log.Printf("register slash commands: %v", err)
			}
		})
	})

	if err = session.Open(); err != nil {
		log.Fatalf("Error opening session: %v", err)
	}
}

func configureDiscordLogger() {
	discordgo.Logger = func(msgLevel, caller int, format string, a ...interface{}) {
		msg := fmt.Sprintf(format, a...)
		if strings.HasPrefix(msg, "received binary:") {
			return
		}
		log.Printf("[DG%d] %s", msgLevel, msg)
	}
}
