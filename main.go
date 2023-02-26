package main

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"
	gpt "github.com/sashabaranov/go-gpt3"
)

type Config struct {
	OpenAI  OpenAIConfig  `json:"OpenAI"`
	Discord DiscordConfig `json:"Discord"`
}

type OpenAIConfig struct {
	APIKey      string `json:"APIKey"`
	Personality string `json:"Personality"`
}

type DiscordConfig struct {
	APIKey string `json:"APIKey"`
}

func readConfig() Config {
	config := Config{}
	configFile, err := os.Open("config.json")
	if err != nil {
		panic(err)
	}
	defer configFile.Close()
	jsonParser := json.NewDecoder(configFile)
	if err = jsonParser.Decode(&config); err != nil {
		panic(err)
	}
	return config
}

func main() {
	config := readConfig()
	if config.OpenAI.APIKey == "" {
		panic("No OpenAI API key provided")
	}
	if config.Discord.APIKey == "" {
		panic("No Discord API key provided")
	}

	personality := "Maru is a cheerful young adult woman. She is a technology enthusiast with a background in childcare and cooking."
	if config.OpenAI.Personality != "" {
		personality = config.OpenAI.Personality
	}

	ctx := context.Background()

	openai := gpt.NewClient(config.OpenAI.APIKey)

	discord, err := discordgo.New("Bot " + config.Discord.APIKey)
	if err != nil {
		panic(err)
	}

	conversation := make(map[string]map[string]string)

	discord.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author.ID == s.State.User.ID {
			return
		}
		prefix := regexp.MustCompile(`(?i)(^\s*maru\b[\s,.;:!]*|[\s,.;:!]*\bmaru\b[^a-z0-9]*\s*$)`)
		if prefix.MatchString(m.Content) {
			query := prefix.ReplaceAllString(m.Content, "")

			channelConversations, ok := conversation[m.ChannelID]
			if !ok {
				channelConversations = make(map[string]string)
				conversation[m.ChannelID] = channelConversations
			}
			authorConversation, ok := channelConversations[m.Author.ID]
			if !ok {
				authorConversation = ""
				channelConversations[m.Author.ID] = authorConversation
			}
			authorConversation += "\nQ: " + query + "\nA:"

			s.ChannelTyping(m.ChannelID)

			if strings.ToLower(strings.TrimSpace(regexp.MustCompile(`(?i)[^a-z0-9\s]+`).ReplaceAllString(query, ""))) == "reset" {
				query = ""
				authorConversation = ""
				s.ChannelMessageSend(m.ChannelID, "Sure, let's start over!")
			}

			if query != "" {
				completion, err := openai.CreateCompletion(ctx, gpt.CompletionRequest{
					Model:     "text-davinci-003",
					Prompt:    personality + "\n\n" + authorConversation,
					MaxTokens: 500,
					TopP:      1,
				})
				if err != nil {
					panic(err)
				}

				responseText := completion.Choices[0].Text
				authorConversation += responseText
				s.ChannelMessageSend(m.ChannelID, responseText)
			}

			channelConversations[m.Author.ID] = authorConversation
		}
	})

	discord.Identify.Intents = discordgo.MakeIntent(discordgo.IntentsAll)

	err = discord.Open()
	if err != nil {
		panic(err)
	}
	defer discord.Close()

	<-make(chan struct{})
}
