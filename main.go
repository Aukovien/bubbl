package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/spotify"
)

const (
	redirectURI         = "http://127.0.0.1:8888/callback" 
	historyFileName     = "spotify_history.json"
	historyPlaylistName = "History"
	configFile          = "config.json"
)

type Config struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

type UserProfile struct {
	ID string `json:"id"`
}

type Artist struct {
	Name string `json:"name"`
}

type SimplifiedTrack struct {

	ID     string `json:"id"`
	Name   string `json:"name"`
	Artist string `json:"artist"`
	URI    string `json:"uri"`
}

type CurrentlyPlaying struct {
	Item struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		URI     string   `json:"uri"`
		Artists []Artist `json:"artists"`
	} `json:"item"`
	IsPlaying bool `json:"is_playing"`
}

type Playlist struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type PagingObject struct {
	Items []json.RawMessage `json:"items"`
	Next  string            `json:"next"`
}

var (
	conf       *oauth2.Config
	token      *oauth2.Token
	history    = make(map[string]SimplifiedTrack)
	userID     string
	playlistID string
)

func main() {
	ctx := context.Background()

	config, err := loadConfig()
	if err != nil {
		log.Fatalf("Error loading config.json. Please create it with your client_id and client_secret. Error: %v", err)
	}

	conf = &oauth2.Config{
		ClientID:     config.ClientID,
		ClientSecret: config.ClientSecret,
		RedirectURL:  redirectURI,
		Scopes: []string{
			"user-read-recently-played",
			"user-read-currently-playing",
			"playlist-modify-public",
			"playlist-modify-private",
			"playlist-read-private",
		},
		Endpoint: spotify.Endpoint,
	}

	client := getClient(ctx)

	err = getUserID(ctx, client)
	if err != nil {
		log.Fatalf("Could not get user ID: %v", err)
	}
	fmt.Printf("Logged in for user: %s\n", userID)

	loadHistory()

	playlistID, err = getOrCreatePlaylist(ctx, client, historyPlaylistName)
	if err != nil {
		log.Fatalf("Could not get or create playlist: %v", err)
	}
	fmt.Printf("Using playlist '%s' (ID: %s)\n", historyPlaylistName, playlistID)

	fmt.Println("Performing initial sync of recently played songs...")
	syncRecentlyPlayed(ctx, client)
	fmt.Println("Initial sync complete.")

	fmt.Println("Monitoring currently playing song... (Checking every 30 seconds)")
	monitorCurrentlyPlaying(ctx, client)
}

func loadConfig() (*Config, error) {
	file, err := os.Open(configFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var config Config
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&config)
	if err != nil {
		return nil, err
	}

	if config.ClientID == "" || config.ClientSecret == "" {
		return nil, fmt.Errorf("config.json is missing client_id or client_secret")
	}

	return &config, nil
}

func getClient(ctx context.Context) *http.Client {
	tokenFile := "spotify_token.json"
	tok, err := tokenFromFile(tokenFile)
	if err == nil {
		fmt.Println("Using cached token.")

		tokenSource := conf.TokenSource(ctx, tok)
		newToken, err := tokenSource.Token()
		if err != nil {
			log.Fatalf("Could not refresh token: %v", err)
		}
		if newToken.AccessToken != tok.AccessToken {
			fmt.Println("Token was refreshed.")
			saveToken(tokenFile, newToken)
			tok = newToken
		}
		return oauth2.NewClient(ctx, tokenSource)
	}
	fmt.Println("Getting new token.")

	ch := make(chan *oauth2.Token)
	server := &http.Server{Addr: ":8888"}
	http.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")

		exchangeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		tok, err := conf.Exchange(exchangeCtx, code)
		if err != nil {
			log.Fatalf("Could not get token: %v", err)
		}

		fmt.Fprint(w, "Authentication successful! You can close this window.")

		go server.Shutdown(context.Background())
		ch <- tok
	})

	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe(): %v", err)
		}
	}()

	url := conf.AuthCodeURL("state", oauth2.AccessTypeOffline)
	fmt.Printf("Your browser should open for Spotify authentication. If not, please visit:\n%v\n", url)
	openBrowser(url)

	tok = <-ch
	saveToken(tokenFile, tok)
	return conf.Client(ctx, tok)
}

func getUserID(ctx context.Context, client *http.Client) error {
	resp, err := client.Get("https://api.spotify.com/v1/me")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to get user profile: %s", string(body))
	}

	var profile UserProfile
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return err
	}
	userID = profile.ID
	return nil
}

func syncRecentlyPlayed(ctx context.Context, client *http.Client) {
	resp, err := client.Get("https://api.spotify.com/v1/me/player/recently-played?limit=50")
	if err != nil {
		log.Printf("Error fetching recently played: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		log.Printf("Error status from recently-played endpoint: %s", string(bodyBytes))
		return
	}

	var result struct {
		Items []struct {
			Track struct {
				ID      string   `json:"id"`
				Name    string   `json:"name"`
				URI     string   `json:"uri"`
				Artists []Artist `json:"artists"`
			} `json:"track"`
		} `json:"items"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("Error decoding recently played: %v", err)
		return
	}

	var newTracks []SimplifiedTrack
	for i := len(result.Items) - 1; i >= 0; i-- {
		item := result.Items[i].Track
		if _, exists := history[item.ID]; !exists {
			track := SimplifiedTrack{
				ID:     item.ID,
				Name:   item.Name,
				Artist: getArtistsString(item.Artists),
				URI:    item.URI,
			}
			history[item.ID] = track
			newTracks = append(newTracks, track)
		}
	}

	if len(newTracks) > 0 {
		fmt.Printf("Found %d new tracks in recent history.\n", len(newTracks))
		saveHistory()
		addTracksToPlaylist(ctx, client, newTracks)
	} else {
		fmt.Println("No new tracks found in recent history.")
	}
}

func monitorCurrentlyPlaying(ctx context.Context, client *http.Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		<-ticker.C
		resp, err := client.Get("https://api.spotify.com/v1/me/player/currently-playing")
		if err != nil {
			log.Printf("Error checking currently playing: %v", err)
			continue
		}

		if resp.StatusCode == http.StatusNoContent {
			resp.Body.Close()
			continue
		}

		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			log.Printf("Error status from currently-playing endpoint: %s", string(bodyBytes))
			resp.Body.Close()
			continue
		}

		var current CurrentlyPlaying
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err := json.Unmarshal(body, &current); err != nil {
			log.Printf("Error decoding currently playing: %v", err)
			continue
		}

		if !current.IsPlaying || current.Item.ID == "" {
			continue
		}

		if _, exists := history[current.Item.ID]; !exists {
			fmt.Printf("New song detected: %s by %s\n", current.Item.Name, getArtistsString(current.Item.Artists))
			track := SimplifiedTrack{
				ID:     current.Item.ID,
				Name:   current.Item.Name,
				Artist: getArtistsString(current.Item.Artists),
				URI:    current.Item.URI,
			}
			history[track.ID] = track
			saveHistory()
			addTracksToPlaylist(ctx, client, []SimplifiedTrack{track})
		}
	}
}

func loadHistory() {
	file, err := os.Open(historyFileName)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("History file not found, will create a new one.")
			return
		}
		log.Fatalf("Error opening history file: %v", err)
	}
	defer file.Close()

	if err := json.NewDecoder(file).Decode(&history); err != nil {
		log.Printf("Could not decode history file, starting fresh: %v", err)
		history = make(map[string]SimplifiedTrack) 
	}
	fmt.Printf("Loaded %d tracks from history file.\n", len(history))
}

func saveHistory() {
	file, err := os.Create(historyFileName)
	if err != nil {
		log.Printf("Error creating history file: %v", err)
		return
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(history); err != nil {
		log.Printf("Error encoding history to file: %v", err)
	}
}


func getArtistsString(artists []Artist) string {
	var names []string
	for _, artist := range artists {
		names = append(names, artist.Name)
	}
	return strings.Join(names, ", ")
}


func getOrCreatePlaylist(ctx context.Context, client *http.Client, name string) (string, error) {
	url := "https://api.spotify.com/v1/me/playlists?limit=50"
	for url != "" {
		resp, err := client.Get(url)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()

		var playlists PagingObject
		if err := json.NewDecoder(resp.Body).Decode(&playlists); err != nil {
			return "", err
		}

		for _, item := range playlists.Items {
			var p Playlist
			if err := json.Unmarshal(item, &p); err == nil {
				if p.Name == name {
					return p.ID, nil
				}
			}
		}
		url = playlists.Next
	}

	fmt.Printf("Playlist '%s' not found. Creating it...\n", name)
	createURL := fmt.Sprintf("https://api.spotify.com/v1/users/%s/playlists", userID)
	playlistData := strings.NewReader(fmt.Sprintf(`{"name":"%s", "public":false, "description":"Listening history logged by Go tool."}`, name))
	req, _ := http.NewRequestWithContext(ctx, "POST", createURL, playlistData)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to create playlist: %s", string(body))
	}

	var newPlaylist Playlist
	if err := json.NewDecoder(resp.Body).Decode(&newPlaylist); err != nil {
		return "", err
	}

	return newPlaylist.ID, nil
}

func addTracksToPlaylist(ctx context.Context, client *http.Client, tracks []SimplifiedTrack) {
	if len(tracks) == 0 {
		return
	}

	url := fmt.Sprintf("https://api.spotify.com/v1/playlists/%s/tracks", playlistID)

	for i := 0; i < len(tracks); i += 100 {
		end := i + 100
		if end > len(tracks) {
			end = len(tracks)
		}
		batch := tracks[i:end]

		var uris []string
		for _, track := range batch {
			uris = append(uris, track.URI)
		}

		body, _ := json.Marshal(map[string][]string{"uris": uris})
		req, _ := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Error adding tracks to playlist: %v", err)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated {
			respBody, _ := io.ReadAll(resp.Body)
			log.Printf("Failed to add tracks to playlist. Status: %s, Body: %s", resp.Status, string(respBody))
		} else {
			fmt.Printf("Successfully added %d track(s) to the playlist.\n", len(batch))
		}
	}
}

func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

func saveToken(path string, token *oauth2.Token) {
	fmt.Printf("Saving credential file to: %s\n", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Fatalf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("unsupported platform")
	}
	if err != nil {
		log.Printf("Could not open browser: %v", err)
	}
}

func init() {
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		fmt.Println("config.json not found.")
		fmt.Println("Please create it with your Spotify Client ID and Secret.")

		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Enter your Client ID: ")
		clientID, _ := reader.ReadString('\n')

		fmt.Print("Enter your Client Secret: ")
		clientSecret, _ := reader.ReadString('\n')

		config := Config{
			ClientID:     strings.TrimSpace(clientID),
			ClientSecret: strings.TrimSpace(clientSecret),
		}

		file, err := os.Create(configFile)
		if err != nil {
			log.Fatalf("Unable to create config.json: %v", err)
		}
		defer file.Close()

		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(config); err != nil {
			log.Fatalf("Unable to write to config.json: %v", err)
		}
		fmt.Println("config.json created successfully.")
	}
}

