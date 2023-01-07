package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
)

var (
	logger *zap.Logger
)

type EmojiState struct {
	guildID          string
	registeredEmojis map[string]*discordgo.Emoji
}

func NewEmojiState(guildID string) EmojiState {
	emojiMap := make(map[string]*discordgo.Emoji)
	return EmojiState{
		guildID:          guildID,
		registeredEmojis: emojiMap,
	}
}

func (es EmojiState) registerEmoji(emoji *discordgo.Emoji) {
	logger.Sugar().Infof("new emoji registered: %#v", emoji)
	es.registeredEmojis[emoji.ID] = emoji
}

func (es EmojiState) checkExists(emoji *discordgo.Emoji) bool {
	_, ok := es.registeredEmojis[emoji.ID]
	return ok
}

type DiscordBot struct {
	config             BotConfig
	session            *discordgo.Session
	guildID2EmojiState map[string]EmojiState
	notifyWorkerChan   chan NotifyRequest
}

type NotifyRequest struct {
	emoji   *discordgo.Emoji
	guildID string
}

type BotConfig struct {
	botToken                   string
	guildID2notifyChannelIDMap map[string]string
	notifyWindow               time.Duration
}

func launchDiscordBot(config BotConfig) (*DiscordBot, error) {
	dg, err := discordgo.New("Bot " + config.botToken)
	if err != nil {
		return nil, err
	}
	emojiStateMap := make(map[string]EmojiState)
	bot := &DiscordBot{
		config:             config,
		session:            dg,
		guildID2EmojiState: emojiStateMap,
	}

	// create bot session
	err = dg.Open()
	logger.Sugar().Info("bot launched")

	// iterate over guilds to initialize emojiState
	logger.Sugar().Infof("available guilds: %d", len(dg.State.Guilds))
	for _, guild := range dg.State.Guilds {
		emojis, err := dg.GuildEmojis(guild.ID)
		if err != nil {
			continue
		}
		if _, ok := emojiStateMap[guild.ID]; !ok {
			emojiStateMap[guild.ID] = NewEmojiState(guild.ID)
		}
		// get all Emojis
		for _, emoji := range emojis {
			emojiStateMap[guild.ID].registerEmoji(emoji)
		}
	}

	// launch the consumer
	bot.notifyWorkerChan = launchNotifyWorker(bot)

	// when emoji is updated, all emojis are sent as an event
	// check emojiState to know which emoji is new one
	dg.AddHandler(func(s *discordgo.Session, geu *discordgo.GuildEmojisUpdate) {
		for _, emoji := range geu.Emojis {
			if !emojiStateMap[geu.GuildID].checkExists(emoji) {
				logger.Sugar().Infof("new emoji!!! %#v", emoji)
				bot.pushEmojiToQueue(geu.GuildID, emoji)
			}
		}
	})

	return bot, err
}

func (bot DiscordBot) pushEmojiToQueue(guildID string, emoji *discordgo.Emoji) {
	req := NotifyRequest{
		emoji:   emoji,
		guildID: guildID,
	}
	bot.notifyWorkerChan <- req
}

func (bot DiscordBot) getNotifyChannelIDFromGuildID(guildID string) (string, bool) {
	id, ok := bot.config.guildID2notifyChannelIDMap[guildID]
	return id, ok
}

func (bot DiscordBot) getGuildByID(guildID string) (*discordgo.Guild, bool) {
	for _, guild := range bot.session.State.Guilds {
		if guild.ID == guildID {
			return guild, true
		}
	}
	return nil, false
}

type NotifyWorker struct {
	sync.RWMutex
	notifyQueue NotifyQueue
}

// queueMap[guildID][]NotifyRequest
type NotifyQueue struct {
	queueMap map[string][]NotifyRequest
}

func NewNotifyQueue() NotifyQueue {
	return NotifyQueue{
		queueMap: make(map[string][]NotifyRequest),
	}
}

func (q NotifyQueue) getMap() map[string][]NotifyRequest {
	return q.queueMap
}

func (q NotifyQueue) clearQueue(guildID string) {
	q.queueMap[guildID] = []NotifyRequest{}
}

func (q NotifyQueue) initializeQueueIfNeeded(guildID string) {
	if _, ok := q.queueMap[guildID]; !ok {
		q.clearQueue(guildID)
	}
}

func (q NotifyQueue) enqueue(req NotifyRequest) {
	q.initializeQueueIfNeeded(req.guildID)
	q.queueMap[req.guildID] = append(q.queueMap[req.guildID], req)
}

func launchNotifyWorker(bot *DiscordBot) (c chan NotifyRequest) {
	worker := NotifyWorker{
		notifyQueue: NewNotifyQueue(),
	}
	channel := make(chan NotifyRequest)
	go func() {
		ticker := time.NewTicker(bot.config.notifyWindow)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C: // group notifyRequests and send a notification every N minutes
				worker.Lock()
				logger.Sugar().Infof("ticked")
				// TODO: to support large number of guilds, make this map to something a form like an LRU to evict empty queue
				for guildID, queue := range worker.notifyQueue.getMap() {
					logger.Sugar().Infof("current queue size for guild id %s is %d", guildID, len(queue))
					if len(queue) <= 0 {
						continue
					}
					// request queue may contains redundant entries when uploaded and updated an emoji entry before submitting
					// overwrite by the latter entry to remove duplicates and send latest state
					uniqueEmojisMap := make(map[string]*discordgo.Emoji)
					for _, req := range queue {
						uniqueEmojisMap[req.emoji.ID] = req.emoji
					}
					uniqueEmojis := make([]*discordgo.Emoji, len(uniqueEmojisMap))
					{
						i := 0
						for _, emoji := range uniqueEmojisMap {
							uniqueEmojis[i] = emoji
							i += 1
						}
					}
					bot.notifyNewEmoji(guildID, uniqueEmojis)
					worker.notifyQueue.clearQueue(guildID)
				}
				worker.Unlock()
			case req := <-channel: // poll notifyRequest triggered by DiscordBot and enqueue the requests
				logger.Sugar().Info("append to notifyQueue")
				worker.Lock()
				worker.notifyQueue.enqueue(req)
				worker.Unlock()
			}
		}
	}()
	return channel
}

func (bot DiscordBot) notifyNewEmoji(guildID string, emojis []*discordgo.Emoji) error {
	guild, ok := bot.getGuildByID(guildID)
	if !ok {
		return fmt.Errorf("the guild (id:%s) is not included in bot session. ignoreing", guildID)
	}
	channelID, ok := bot.getNotifyChannelIDFromGuildID(guildID)
	if !ok {
		return fmt.Errorf("the guild (id:%s) does not registered notify channel. ignoreing", guildID)
	}

	summaryBuffer := make([]string, len(emojis))
	for i, emoji := range emojis {
		if bot.guildID2EmojiState[guildID].checkExists(emoji) {
			// there is potentially race problem by the emoji update event handler when notifyQueue process completed and until registerEmoji, hence we remove duplicates that already notified emojis here
			continue
		}
		summaryBuffer[i] = fmt.Sprintf("%s (`:%s:`)", emoji.MessageFormat(), emoji.Name)
		bot.guildID2EmojiState[guildID].registerEmoji(emoji)
	}
	embed := &discordgo.MessageEmbed{
		Title:       "New Emoji",
		Color:       0x5ae9ff,
		Description: fmt.Sprintf(":new: **%d emoji(s)** are added to the server!\n\n%s", len(emojis), strings.Join(summaryBuffer, "\n")),
		Footer: &discordgo.MessageEmbedFooter{
			Text: guild.Name,
		},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	bot.session.ChannelMessageSendEmbed(channelID, embed)
	return nil
}

func (bot DiscordBot) closeDiscordBot() {
	bot.session.Close()
}

func getEmojiImageURLFromEmojiID(emoji *discordgo.Emoji) (string, error) {
	var result string
	if emoji == nil {
		return "", fmt.Errorf("emoji must not be nil")
	}
	if emoji.Animated {
		result = fmt.Sprintf("https://cdn.discordapp.com/emojis/%s.gif", emoji.ID)
	} else {
		result = fmt.Sprintf("https://cdn.discordapp.com/emojis/%s.png", emoji.ID)
	}
	logger.Sugar().Infof("Emoji URL is: %s", result)
	return result, nil
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	logger, _ = zap.NewProduction()
	defer func() {
		_ = logger.Sync()
	}()
	go func() {
		logger.Sugar().Info(http.ListenAndServe("localhost:6060", nil))
	}()

	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		logger.Sugar().Error("BOT_TOKEN is required")
		os.Exit(1)
	}

	// TODO: make this configurale by a slack command
	config := BotConfig{
		botToken: botToken,
		guildID2notifyChannelIDMap: map[string]string{
			"412059076449533974": "761560679018004521",  // ksswre
			"786887293322788874": "1060908636345470986", // rougai
		},
		notifyWindow: 5 * time.Minute,
	}
	bot, _ := launchDiscordBot(config)
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
	bot.closeDiscordBot()
}
