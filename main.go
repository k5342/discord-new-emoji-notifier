package main

import (
	"encoding/json"
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
	config             *BotConfig
	session            *discordgo.Session
	guildID2EmojiState map[string]EmojiState
	notifyWorkerChan   chan NotifyRequest
	registeredCommands []*discordgo.ApplicationCommand
}

type NotifyRequest struct {
	emoji   *discordgo.Emoji
	guildID string
}

type BotConfig struct {
	sync.RWMutex
	botToken                   string
	configFileToChannelList    string
	guildID2notifyChannelIDMap map[string]string
	notifyWindow               time.Duration
}

func (bc *BotConfig) restoreChannelIDsFromFile() error {
	bc.Lock()
	defer bc.Unlock()
	var channelIDMap map[string]string
	_, err := os.Stat(bc.configFileToChannelList)
	if err != nil {
		// file is not found
		bc.guildID2notifyChannelIDMap = map[string]string{}
		return err
	}
	bytes, err := os.ReadFile(bc.configFileToChannelList)
	if err != nil {
		bc.guildID2notifyChannelIDMap = map[string]string{}
		return err
	}
	err = json.Unmarshal(bytes, &channelIDMap)
	if err != nil {
		bc.guildID2notifyChannelIDMap = map[string]string{}
		return err
	}
	bc.guildID2notifyChannelIDMap = channelIDMap
	return nil
}

func (bc *BotConfig) persistChannelIDsToFile() error {
	bc.RLock()
	defer bc.RUnlock()
	bytes, err := json.Marshal(bc.guildID2notifyChannelIDMap)
	if err != nil {
		return nil
	}
	err = os.WriteFile(bc.configFileToChannelList, bytes, 0644)
	if err != nil {
		return nil
	}
	return nil
}

func launchDiscordBot(config *BotConfig) (*DiscordBot, error) {
	dg, err := discordgo.New("Bot " + config.botToken)
	if err != nil {
		return nil, err
	}
	err = config.restoreChannelIDsFromFile()
	if err != nil {
		logger.Sugar().Warn("failed on restore channel ids", err)
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

	// handler for slash commands to setup/remove notification channel in a guild
	dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		var msg string
		commandName := i.ApplicationCommandData().Name
		switch commandName {
		case "register":
			err := bot.registerNotificationChannel(i.GuildID, i.ChannelID)
			if err == nil {
				msg = "okay, I will notify here for new emojis!"
			} else {
				msg = fmt.Sprintf("hmm, something went to wrong: %s", err)
			}
		case "unregister":
			err := bot.unregisterNotificationChannel(i.GuildID, i.ChannelID)
			if err == nil {
				msg = "unregistered!"
			} else {
				msg = fmt.Sprintf("hmm, something went to wrong: %s", err)
			}
		default:
			msg = "invalid command :("
		}
		err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: msg,
			},
		})
		if err != nil {
			logger.Sugar().Error(err)
		}
	})

	bot.registerSlashCommands()

	return bot, err
}

func (bot DiscordBot) registerNotificationChannel(guildID string, channelID string) error {
	// check whether the given channel is accessible via bot's permission or not
	_, err := bot.session.Channel(channelID)
	if err != nil {
		return fmt.Errorf("could not find out the channel you've requested (might be wrong permissions?)")
	}
	bot.config.Lock()
	defer bot.config.Unlock()
	bot.config.guildID2notifyChannelIDMap[guildID] = channelID
	logger.Sugar().Infof("registered: guild %s -> channel %s", guildID, channelID)
	return nil
}

func (bot DiscordBot) unregisterNotificationChannel(guildID string, channelID string) error {
	bot.config.Lock()
	defer bot.config.Unlock()
	cid, ok := bot.config.guildID2notifyChannelIDMap[guildID]
	if !ok {
		return fmt.Errorf("no channel registered")
	}
	if cid != channelID {
		return fmt.Errorf("this channel is not registered as the notification channel")
	}
	delete(bot.config.guildID2notifyChannelIDMap, guildID)
	logger.Sugar().Infof("unregistered: guild %s: remove channel %s", guildID, channelID)
	return nil
}

func (bot DiscordBot) pushEmojiToQueue(guildID string, emoji *discordgo.Emoji) {
	req := NotifyRequest{
		emoji:   emoji,
		guildID: guildID,
	}
	bot.notifyWorkerChan <- req
}

func (bot DiscordBot) getNotifyChannelIDFromGuildID(guildID string) (string, bool) {
	bot.config.RLock()
	id, ok := bot.config.guildID2notifyChannelIDMap[guildID]
	bot.config.RUnlock()
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

func (bot DiscordBot) registerSlashCommands() {
	// TODO: set default permission for register/unregister command to limit access to moderators who can edit channels only
	var permission int64
	// if we use default permissions below, it does not consider implicit permissions such as the owner permissions.
	// as a result, this requires that guild owner must join something dedicated role to get these permissions explicitly.
	// we don't need to define a default permission here for now because slash command can be limited using guild's permission.
	// permission |= discordgo.PermissionAdministrator
	// permission |= discordgo.PermissionManageChannels
	commands := []*discordgo.ApplicationCommand{
		{
			Name:                     "register",
			Description:              "make this channel to a notification channel",
			DefaultMemberPermissions: &permission,
		},
		{
			Name:                     "unregister",
			Description:              "stop to notify here",
			DefaultMemberPermissions: &permission,
		},
	}
	bot.registeredCommands = make([]*discordgo.ApplicationCommand, len(commands))
	for idx, val := range commands {
		registered, err := bot.session.ApplicationCommandCreate(bot.session.State.User.ID, "", val)
		if err == nil {
			logger.Sugar().Infof("created a command '%#v'", val.Name)
		} else {
			logger.Sugar().Errorf("cannot create command '%#v': %#v", val.Name, err)
		}
		bot.registeredCommands[idx] = registered
	}
}

func (bot DiscordBot) unregisterSlashCommands() {
	for _, val := range bot.registeredCommands {
		err := bot.session.ApplicationCommandDelete(bot.session.State.User.ID, "", val.ID)
		if err == nil {
			logger.Sugar().Infof("deleted a command: %s", val.Name)
		} else {
			logger.Sugar().Errorf("cannot delete command %s: %v", val.Name, err)
		}
	}
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
					err := bot.notifyNewEmoji(guildID, uniqueEmojis)
					if err != nil {
						logger.Sugar().Warnf("failed on notifyNewEmoji: %s", err)
						continue
					}
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
	_, err := bot.session.ChannelMessageSendEmbed(channelID, embed)
	if err != nil {
		return fmt.Errorf("failed on ChannelMessageSendEmbed: %s", err)
	}
	return nil
}

func (bot DiscordBot) closeDiscordBot() {
	err := bot.config.persistChannelIDsToFile()
	if err != nil {
		logger.Sugar().Error("failed on persist channel ids", err)
	}
	bot.unregisterSlashCommands()
	bot.session.Close()
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

	config := BotConfig{
		botToken:                botToken,
		configFileToChannelList: "./channels.json",
		notifyWindow:            5 * time.Minute,
	}
	bot, _ := launchDiscordBot(&config)
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
	bot.closeDiscordBot()
}
