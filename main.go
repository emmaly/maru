package main

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	gpt "github.com/sashabaranov/go-gpt3"
)

type Config struct {
	ConsoleLog bool          `json:"ConsoleLog"`
	OpenAI     OpenAIConfig  `json:"OpenAI"`
	Discord    DiscordConfig `json:"Discord"`
}

type OpenAIConfig struct {
	APIKey      string  `json:"APIKey"`
	MaxTokens   int     `json:"MaxTokens"`
	TopP        float32 `json:"TopP"`
	Model       string  `json:"Model"`
	Personality string  `json:"Personality"`
}

type DiscordConfig struct {
	mu                 sync.Mutex
	APIKey             string                     `json:"APIKey"`
	Channels           map[string]*DiscordChannel `json:"Channels"`
	SharedConversation bool                       `json:"SharedConversation"`
}

type DiscordChannel struct {
	mu                 sync.Mutex
	ID                 string                   `json:"ID"`
	SharedConversation bool                     `json:"SharedConversation"`
	Model              string                   `json:"Model"`
	Personality        string                   `json:"Personality"`
	Conversations      map[string]*Conversation `json:"Conversations"`
}

type Conversation struct {
	mu          sync.Mutex
	Model       string     `json:"Model"`
	Personality string     `json:"Personality"`
	Messages    []*Message `json:"Messages"`
}

type Message struct {
	Time    time.Time `json:"Time"`
	Author  string    `json:"Author"`
	Content string    `json:"Content"`
}

func readConfig() *Config {
	config := &Config{}
	configFile, err := os.Open("config.json")
	if err != nil {
		panic(err)
	}
	defer configFile.Close()
	jsonParser := json.NewDecoder(configFile)
	if err = jsonParser.Decode(config); err != nil {
		panic(err)
	}

	// set defaults
	if config.OpenAI.Model == "" {
		config.OpenAI.Model = "text-davinci-003"
	}
	if config.OpenAI.MaxTokens == 0 {
		config.OpenAI.MaxTokens = 100
	}
	if config.OpenAI.TopP == 0 {
		config.OpenAI.TopP = 1
	}
	if config.OpenAI.Personality == "" {
		config.OpenAI.Personality = "Maru is a cheerful young adult. She is also a cat."
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

	if config.ConsoleLog {
		log.SetOutput(os.Stdout)
	} else {
		log.SetOutput(ioutil.Discard)
	}

	ctx := context.Background()

	openai := gpt.NewClient(config.OpenAI.APIKey)

	discord, err := discordgo.New("Bot " + config.Discord.APIKey)
	if err != nil {
		panic(err)
	}

	discord.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if !m.Author.Bot { // we don't care about bots
			messageCreate(config, s, m, ctx, openai)
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

func getConversation(config *Config, channelID string, authorID string) (*Conversation, *DiscordChannel) {
	config.Discord.mu.Lock()
	defer config.Discord.mu.Unlock()
	channel, ok := config.Discord.Channels[channelID]
	if !ok {
		config.Discord.Channels[channelID] = &DiscordChannel{
			ID:                 channelID,
			SharedConversation: config.Discord.SharedConversation,
			Conversations:      make(map[string]*Conversation),
		}
		channel = config.Discord.Channels[channelID]
	}
	if channel.SharedConversation {
		authorID = "" // shared conversation uses empty authorID
	}

	channel.mu.Lock()
	defer channel.mu.Unlock()
	conversation, ok := channel.Conversations[authorID]
	if !ok {
		channel.Conversations = make(map[string]*Conversation)
		channel.Conversations[authorID] = &Conversation{
			Messages: make([]*Message, 0),
		}
		conversation = channel.Conversations[authorID]
	}
	return conversation, channel
}

func (c *Conversation) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Messages = make([]*Message, 0)
}

func (c *Conversation) addMessage(timestamp time.Time, author string, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Messages == nil {
		c.Messages = make([]*Message, 0)
	}
	c.Messages = append(c.Messages, &Message{
		Time:    timestamp,
		Author:  author,
		Content: content,
	})
}

func (c *Conversation) getPrompt(personality string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	prompt := personality + "\n\n"
	for _, m := range c.Messages {
		prompt += m.Author + ": " + m.Content + "\n"
	}
	return prompt
}

func messageCreate(config *Config, s *discordgo.Session, m *discordgo.MessageCreate, ctx context.Context, openai *gpt.Client) {
	if m.Author.ID == s.State.User.ID {
		return // we don't care about ourselves
	}

	isPM := m.GuildID == "" // we're in a PM
	amMentioned := false
	content := m.Content
	for _, u := range m.Mentions {
		if u.ID == s.State.User.ID { // we're mentioned
			amMentioned = true
		}
		content = regexp.MustCompile(`(^\s*)?<@(&!)?`+u.ID+`>(\s*$)?`).ReplaceAllString(content, u.Username)
	}

	if !isPM && !amMentioned {
		return
	}

	log.Printf("\n	Username  : %s\n	Author    : %s\n	Channel   : %s\n	Content 1 : %s\n	Content 2 : %s\n	Mentioned : %t\n	IsPrivate : %t\n\n", m.Author.Username, m.Author.ID, m.ChannelID, m.Content, content, amMentioned, isPM)

	// clean the content into a query
	query := strings.TrimSpace(content)

	// get the conversation, or create a new one
	conversation, channel := getConversation(config, m.ChannelID, m.Author.ID)

	// set chatbot personality/backstory
	personality := config.OpenAI.Personality
	if conversation.Personality != "" {
		personality = conversation.Personality
	} else if channel.Personality != "" {
		personality = channel.Personality
	}

	// set OpenAI Model
	model := config.OpenAI.Model
	if conversation.Model != "" {
		model = conversation.Model
	} else if channel.Model != "" {
		model = channel.Model
	}

	// show the user that we've heard them and are preparing a response
	s.ChannelTyping(m.ChannelID)

	// if the message is "maru reset", then we'll reset the conversation
	if regexp.MustCompile(`(?i)^\s*` + s.State.User.Username + `\s*[^a-z0-9]+\s*reset\s*[^a-z0-9]*\s*$`).MatchString(query) {
		conversation.reset()
		s.ChannelMessageSend(m.ChannelID, "Sure, let's start over!")
		return
	}

	// if the message is "maru personality set-conversation <personality>", then we'll set the personality for this conversation
	personalitySetPrefix := regexp.MustCompile(`(?i)^\s*` + s.State.User.Username + `\s*[^a-z0-9]+\s*personality[-\s]*set[-]*conversation\s*[^a-z0-9]*\s*`)
	if personalitySetPrefix.MatchString(query) {
		conversation.Personality = strings.TrimSpace(personalitySetPrefix.ReplaceAllString(query, ""))
		s.ChannelMessageSend(m.ChannelID, "Got it! I'll remember that.")
		query = s.State.User.Username + " personality get-conversation"
	}

	// if the message is "maru personality set-channel <personality>", then we'll set the personality for this channel
	personalitySetChannelPrefix := regexp.MustCompile(`(?i)^\s*` + s.State.User.Username + `\s*[^a-z0-9]+\s*personality[-\s]*set[-]*channel\s*[^a-z0-9]*\s*`)
	if personalitySetChannelPrefix.MatchString(query) {
		channel.Personality = strings.TrimSpace(personalitySetChannelPrefix.ReplaceAllString(query, ""))
		s.ChannelMessageSend(m.ChannelID, "Got it! I'll remember that.")
		query = s.State.User.Username + " personality get-channel"
	}

	// if the message is "maru personality set-global <personality>", then we'll set the personality for the entire bot
	personalitySetGlobalPrefix := regexp.MustCompile(`(?i)^\s*` + s.State.User.Username + `\s*[^a-z0-9]+\s*personality[-\s]*set[-]*global\s*[^a-z0-9]*\s*`)
	if personalitySetGlobalPrefix.MatchString(query) {
		config.OpenAI.Personality = strings.TrimSpace(personalitySetGlobalPrefix.ReplaceAllString(query, ""))
		s.ChannelMessageSend(m.ChannelID, "Got it! I'll remember that.")
		query = s.State.User.Username + " personality get-global"
	}

	// if the message is "maru personality (get)?", then we'll show the personality, wherever it's set/inherited from
	if regexp.MustCompile(`(?i)^\s*` + s.State.User.Username + `\s*[^a-z0-9]+\s*personality([-\s]*get)?\s*[^a-z0-9]*\s*$`).MatchString(query) {
		personality := config.OpenAI.Personality
		personalitySource := "globally"
		if conversation.Personality != "" {
			personality = conversation.Personality
			personalitySource = "only on this conversation"
		} else if channel.Personality != "" {
			personality = channel.Personality
			personalitySource = "for this channel only"
		}
		if personality == "" {
			personality = "(not set at all)"
			personalitySource = "nowhere at all.."
		}
		s.ChannelMessageSend(m.ChannelID, "My personality is:\n```"+personality+"```This is set "+personalitySource+".")
		return
	}

	// if the message is "maru personality get-conversation", then we'll show the personality for this conversation
	if regexp.MustCompile(`(?i)^\s*` + s.State.User.Username + `\s*[^a-z0-9]+\s*personality[-\s]*get[-]*conversation\s*[^a-z0-9]*\s*$`).MatchString(query) {
		personality := conversation.Personality
		if personality == "" {
			personality = "(not set at the conversation level)"
		}
		s.ChannelMessageSend(m.ChannelID, "My personality for this conversation is:\n```"+personality+"```")
		return
	}

	// if the message is "maru personality get-channel", then we'll show the personality for this channel
	if regexp.MustCompile(`(?i)^\s*` + s.State.User.Username + `\s*[^a-z0-9]+\s*personality[-\s]*get[-]*channel\s*[^a-z0-9]*\s*$`).MatchString(query) {
		personality := channel.Personality
		if personality == "" {
			personality = "(not set at the channel level)"
		}
		s.ChannelMessageSend(m.ChannelID, "My personality for this channel is:\n```"+personality+"```")
		return
	}

	// if the message is "maru personality get-global", then we'll show the personality for the entire bot
	if regexp.MustCompile(`(?i)^\s*` + s.State.User.Username + `\s*[^a-z0-9]+\s*personality[-\s]*get[-]*global\s*[^a-z0-9]*\s*$`).MatchString(query) {
		personality := config.OpenAI.Personality
		if personality == "" {
			personality = "(not set at the global level)"
		}
		s.ChannelMessageSend(m.ChannelID, "My personality for the entire bot is:\n```"+personality+"```")
		return
	}

	// if we still have a query left, let's send it to OpenAI
	if query != "" {
		// append the new message to the conversation
		conversation.addMessage(m.Timestamp, m.Author.Username, query)

		// get the entire prompt for OpenAI
		prompt := conversation.getPrompt(personality)

		// send the prompt to OpenAI
		completion, err := openai.CreateCompletion(ctx, gpt.CompletionRequest{
			Model:     model,
			Prompt:    prompt,
			MaxTokens: config.OpenAI.MaxTokens,
			TopP:      config.OpenAI.TopP,
		})
		if err != nil {
			panic(err)
		}

		// remove the unwanted username prefix from the response
		responseText := regexp.MustCompile(`(?i)^\s*`+s.State.User.Username+`\s*:\s*`).ReplaceAllString(completion.Choices[0].Text, "")

		// store the message in the conversation
		conversation.addMessage(time.Now(), s.State.User.Username, responseText)

		// send the response to the channel
		s.ChannelMessageSend(m.ChannelID, responseText)
	}

	// log the conversation as it stands
	log.Println("\nConversation:\n", conversation.getPrompt(personality))
}
