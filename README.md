# GreeRetransmitter

A one-to-many discord-to-discord one-way voice bridge (I love fancy words).

This is just a bot that listens to voice data in one channel and broadcasts it to multiple channels.

The need for this bot came from playing an MMORPG without paying for a TeamSpeak license, and Discord refusing to add a whisper feature. Hence the name (Gree was the shotcaller of the guild this was made for).

## Running the bot

### Simple

To run the bot, take a look in `config.dev.yaml` (default config file), customize it to your liking according to the schema in `config/config.go`, and run

```sh
go run .
```

### Advanced

You can run this bot in Docker. It does not have any external dependencies, does not need port forwarding, and does not use any fancy features, so you can just do

```sh
docker build -t greeretransmitter:latest .
docker run --name greeretransmitter --rm greeretransmitter:latest
```

To configure the bot, you can:

- Edit the config before building the image; or
- Add the environment variable `CONFIG_PATH` (with `-e` flag to docker run) that contains the path to the config file, which itself can be mounted with `-v` flag to docker run.
