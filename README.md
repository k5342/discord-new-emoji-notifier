# discord-new-emoji-notifier
A simple discord bot to notify new emojis

## Example
![](https://user-images.githubusercontent.com/1993005/211133828-516fe67d-e295-42b1-bdf3-3d4095c1bdf9.png)

## Installation & Run
```
cp .env{.example,}
$EDITOR .env # issue a BOT_TOKEN from Discord Developer Portal
go mod tidy
go run main.go
```

An invitation link is formatted as here; using client_id you can get from Discord Developer Portal:
```
https://discord.com/oauth2/authorize?client_id=<client_id>&scope=bot&permissions=2048
```

## TODO(s)
- rename to a cool name (current: discord-new-emoji-notifier)
- make a notification channel configurable
  - currently we need to define notification channel id statically in this code
  - to make this configurable, we need to ensure a invitation command to select channel and persistent the configuration as a file

## Design
To group notifications each polling window, this implementation follows Producer-Consumer pattern

### Producers: watch gateway event and register notification queue
1. Watch GuildEmojiUpdate sent from Gateway Event
1. Triggers pushEmojiToQueue() to push notifyRequest

### Consumers: send a notification on new emojis to a pre-defined channel
1. Get the notifyRequest pushed by the producer
1. Register the notifyRequest to a notifyQueue
1. (every N minutes) Get requests from the notifyQueue and send a message to the channel
