package main

import "github.com/bwmarrin/discordgo"

// ---------------------------------------------------------------------------
// Slash command definitions
// ---------------------------------------------------------------------------

var slashCommands = []*discordgo.ApplicationCommand{
	{
		Name:        "play",
		Description: "Play music from YouTube Music",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:         discordgo.ApplicationCommandOptionString,
				Name:         "query",
				Description:  "Search terms, or paste a watch / playlist URL",
				Required:     true,
				Autocomplete: true,
			},
		},
	},
	{Name: "skip", Description: "Skip the current track"},
	{Name: "stop", Description: "Stop playback and clear the queue"},
	{Name: "shuffle", Description: "Shuffle the queue"},
	{
		Name:        "remove",
		Description: "Remove a track from the queue",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "index",
				Description: "Queue number to remove",
				Required:    true,
				MinValue:    &queuePageMinValue,
			},
		},
	},
	{
		Name:        "move",
		Description: "Move a queued track to another position",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "from",
				Description: "Queue number to move",
				Required:    true,
				MinValue:    &queuePageMinValue,
			},
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "to",
				Description: "New queue position",
				Required:    true,
				MinValue:    &queuePageMinValue,
			},
		},
	},
	{Name: "clear", Description: "Clear queued tracks without stopping playback"},
	{
		Name:        "queue",
		Description: "Show what is playing and what is queued",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "page",
				Description: "Queue page to show",
				Required:    false,
				MinValue:    &queuePageMinValue,
			},
		},
	},
	{
		Name:        "autoplay",
		Description: "Toggle related-track autoplay when the queue ends",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionBoolean,
				Name:        "enabled",
				Description: "Set autoplay on or off. Omit to toggle.",
				Required:    false,
			},
		},
	},
}
