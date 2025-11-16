# bubbl

CLI history logger tool for spotify.

## 🌟 Features
- Real-time Monitoring
- Catch-up Sync: Performs a full sync of your recently played history to catch any songs missed between polls/downtime.
- Automatic Playlist Creation: Creates a "History" playlist by default if it doesn't exist.
- Intelligent Sync: On startup, it reads all tracks from your "History" playlist to prevent adding duplicates.
- Local Cache: Saves a `.json` file to keep track of processed songs.

## ⚡ Requirements
- Go **1.21+**
- Git
- Spotify Account.

## 🚀 Setup
### Create a Spotify App
You need a `Client ID` and `Client Secret` to use **bubbl**.
1. Go to the [Spotify Developer Dashboard](https://developer.spotify.com/).
2. Log in and click **"Create app"**.
3. Give your app a name and a description.
4. Once created, you will see your `Client ID` and `Client Secret`. You will need these in a moment.
5. Go to **"App settings"**.
6. Find the **"Redirect URIs"** section.
7. Add this exact URL: `http://127.0.0.1:8888/callback`
8. Click **"Save"** at the bottom of the page.

### Clone the Repository
```bash
# clone
git clone https://github.com/Aukovien/bubbl.git

# navigate into the new directory
cd bubbl
```

### Run
1. Now run the program
```bash
go run .
```
2. You will be prompted to enter the `Client ID` and `Client Secret` you got from Step 1. 
3. Log in via the new tab opened on your browser. Click **"Agree"**.

That's it! You can just leave it running in a terminal window.
