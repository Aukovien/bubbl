<div align="center">
  <img src="bubbl.gif">
</div>
<br>
    
CLI history logger for Spotify.

## 🌟 Features
- Polls every 30 seconds to catch what you're listening to
- Syncs your recently played history on startup to fill any gaps
- Creates a "History" playlist automatically if one doesn't exist
- Reads existing playlist tracks on startup so nothing gets added twice
- Saves a local `.json` cache to track everything it's seen

## ⚡ Requirements
- Go 1.21+
- A Spotify account

## 🚀 Setup

### 1. Create a Spotify App
Go to the [Spotify Developer Dashboard](https://developer.spotify.com/) and create a new app. Once created, go to App Settings and add this redirect URI

```
http://127.0.0.1:8888/callback
```

You'll need the Client ID and Client Secret for the next step.

### 2. Clone and run

```bash
git clone https://github.com/Aukovien/bubbl.git
cd bubbl
go run .
```

Paste in your Client ID and Client Secret when prompted, then log in via the browser tab that opens.

After that first login your token is cached locally so you won't need to authenticate again. Just leave it running in a terminal.
