package main

import (
	"bufio"
	"bytes"
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

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/spotify"
)

// --- Constants ---
const (
	redirectURI         = "http://127.0.0.1:8888/callback"
	historyFileName     = "spotify_history.json"
	historyPlaylistName = "History"
	configFile          = "config.json"
	monitorInterval     = 30 * time.Second
	syncInterval        = 30 * time.Minute
)

// --- Structs ---
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

type PlaylistTrack struct {
	Track struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		URI     string   `json:"uri"`
		Artists []Artist `json:"artists"`
	} `json:"track"`
}

type PlaylistTracksPaging struct {
	Items []PlaylistTrack `json:"items"`
	Next  string          `json:"next"`
}

// --- Bubble Tea Model ---
type model struct {
	config     *oauth2.Config
	client     *http.Client
	userID     string
	playlistID string
	history    map[string]SimplifiedTrack
	spinner    spinner.Model
	status     string
	logs       []string
	err        error
	quitting   bool
}

// --- Bubble Tea Messages (Events) ---
type setupDoneMsg struct {
	client     *http.Client
	userID     string
	playlistID string
	history    map[string]SimplifiedTrack
}

type newTracksMsg struct{ tracks []SimplifiedTrack }
type tracksAddedMsg struct{ count int }
type logMsg string
type errorMsg struct{ err error }
type monitorTickMsg struct{}
type syncTickMsg struct{}

func monitorTick() tea.Cmd {
	return tea.Tick(monitorInterval, func(t time.Time) tea.Msg {
		return monitorTickMsg{}
	})
}

func syncTick() tea.Cmd {
	return tea.Tick(syncInterval, func(t time.Time) tea.Msg {
		return syncTickMsg{}
	})
}

// --- Main ---
func main() {
	m := initialModel()
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Alas, there's been an error: %v", err)
		os.Exit(1)
	}
}

func initialModel() model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	return model{
		spinner: s,
		status:  "Initializing and authenticating...",
		history: make(map[string]SimplifiedTrack),
		logs:    make([]string, 0, 15),
	}
}

// --- Tea Init ---
func (m model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		doInitialSetup,
	)
}

func syncPlaylistToHistory(client *http.Client, playlistID string, history map[string]SimplifiedTrack) (map[string]SimplifiedTrack, int, error) {
	url := fmt.Sprintf("https://api.spotify.com/v1/playlists/%s/tracks?limit=50", playlistID)
	newTracksFound := 0

	for url != "" {
		resp, err := client.Get(url)
		if err != nil {
			return history, newTracksFound, err
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close() // Close body -- non-OK status
			return history, newTracksFound, fmt.Errorf("failed to get playlist tracks: %s", resp.Status)
		}

		var playlistPage PlaylistTracksPaging
		if err := json.NewDecoder(resp.Body).Decode(&playlistPage); err != nil {
			resp.Body.Close() // Close body -- decode error
			return history, newTracksFound, err
		}

		// close body inside loop
		resp.Body.Close()

		for _, item := range playlistPage.Items {
			if _, exists := history[item.Track.ID]; !exists && item.Track.ID != "" {
				history[item.Track.ID] = SimplifiedTrack{
					ID:     item.Track.ID,
					Name:   item.Track.Name,
					Artist: getArtistsString(item.Track.Artists),
					URI:    item.Track.URI,
				}
				newTracksFound++
			}
		}
		url = playlistPage.Next // Get next page URL
	}
	return history, newTracksFound, nil
}

// --- Event loop ---
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := msg.(errorMsg); ok {
		m.err = msg.err
		m.status = "A critical error occurred. Press 'q' to quit."
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		default:
			return m, nil
		}

	case setupDoneMsg:
		m.client = msg.client
		m.userID = msg.userID
		m.playlistID = msg.playlistID
		m.history = msg.history
		m.status = fmt.Sprintf("Monitoring user '%s' on playlist '%s'", m.userID, historyPlaylistName)
		m.addLog(fmt.Sprintf("Loaded/Synced %d tracks from local cache.", len(m.history)))
		m.addLog("Performing initial sync of recently played songs...")
		return m, tea.Batch(
			monitorTick(),
			syncTick(),
			syncRecentlyPlayed(m.client),
		)

	case monitorTickMsg:
		var cmds []tea.Cmd
		cmds = append(cmds, monitorTick())
		if m.client != nil {
			cmds = append(cmds, checkCurrentlyPlaying(m.client))
		}
		return m, tea.Batch(cmds...)

	case syncTickMsg:
		var cmds []tea.Cmd
		cmds = append(cmds, syncTick())
		if m.client != nil {
			cmds = append(cmds, syncRecentlyPlayed(m.client))
		}
		return m, tea.Batch(cmds...)

	case newTracksMsg:
		var newTracks []SimplifiedTrack
		var tracksToAddCmd tea.Cmd
		for _, track := range msg.tracks {
			if _, exists := m.history[track.ID]; !exists {
				m.history[track.ID] = track
				newTracks = append(newTracks, track)
				m.addLog(fmt.Sprintf("New track: %s - %s", track.Artist, track.Name))
			}
		}
		if len(newTracks) > 0 {
			tracksToAddCmd = addTracksToPlaylist(m.client, m.playlistID, newTracks)
			return m, tea.Batch(tracksToAddCmd, saveHistory(m.history))
		}
		return m, nil

	case tracksAddedMsg:
		m.addLog(fmt.Sprintf("Successfully added %d track(s) to playlist.", msg.count))
		return m, nil

	case logMsg:
		m.addLog(string(msg))
		return m, nil

	default:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
}

func (m *model) addLog(s string) {
	logLine := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), s)
	m.logs = append(m.logs, logLine)
	if len(m.logs) > 15 {
		m.logs = m.logs[1:]
	}
}

// --- Render ---
func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("\n%s\n\n%v\n\nPress 'q' to quit.", m.status, m.err)
	}
	if m.quitting {
		return "Shutting down...\n"
	}
	s := "Spotify History Monitor\n\n"
	if m.client == nil {
		s += m.spinner.View() + " " + m.status + "\n\n"
	} else {
		s += m.status + "\n\n"
	}
	s += "--- Activity Log ---\n"
	for _, log := range m.logs {
		s += log + "\n"
	}
	s += "\nPress 'q' to quit."
	return s
}

// --- Commands ---
func doInitialSetup() tea.Msg {
	ctx := context.Background()
	config, err := loadConfig()

	if err != nil {
		return errorMsg{fmt.Errorf("error loading config.json: %w", err)}
	}

	conf := &oauth2.Config{
		ClientID:     config.ClientID,
		ClientSecret: config.ClientSecret,
		RedirectURL:  redirectURI,
		Scopes: []string{
			"user-read-recently-played", "user-read-currently-playing",
			"playlist-modify-public", "playlist-modify-private", "playlist-read-private",
			"playlist-read-collaborative",
		},
		Endpoint: spotify.Endpoint,
	}

	client, err := getClient(ctx, conf)

	if err != nil {
		return errorMsg{fmt.Errorf("could not get http client: %w", err)}
	}

	userID, err := getUserID(ctx, client)

	if err != nil {
		return errorMsg{fmt.Errorf("could not get user ID: %w", err)}
	}

	history := loadHistory()
	playlistID, err := getOrCreatePlaylist(ctx, client, userID, historyPlaylistName)

	if err != nil {
		return errorMsg{fmt.Errorf("could not get/create playlist: %w", err)}
	}

	history, newTracksFound, err := syncPlaylistToHistory(client, playlistID, history)

	if err != nil {
		return errorMsg{fmt.Errorf("could not sync with playlist: %w", err)}
	}

	if newTracksFound > 0 {
		fmt.Printf("Synced %d tracks from playlist to local history.\n", newTracksFound)
		saveMsg := saveHistory(history)()
		if saveMsg != nil {
			if err, ok := saveMsg.(errorMsg); ok {
				return err
			}
		}
	}

	return setupDoneMsg{
		client:     client,
		userID:     userID,
		playlistID: playlistID,
		history:    history,
	}
}

func checkCurrentlyPlaying(client *http.Client) tea.Cmd {
	return func() tea.Msg {
		resp, err := client.Get("https://api.spotify.com/v1/me/player/currently-playing")

		if err != nil {
			return logMsg(fmt.Sprintf("Error checking currently playing: %v", err))
		}

		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNoContent {
			return nil
		}

		if resp.StatusCode != http.StatusOK {
			return logMsg("Error status from currently-playing endpoint")
		}

		var current CurrentlyPlaying

		if err := json.NewDecoder(resp.Body).Decode(&current); err != nil {
			return logMsg(fmt.Sprintf("Error decoding currently playing: %v", err))
		}

		if !current.IsPlaying || current.Item.ID == "" {
			return nil
		}

		track := SimplifiedTrack{
			ID:     current.Item.ID,
			Name:   current.Item.Name,
			Artist: getArtistsString(current.Item.Artists),
			URI:    current.Item.URI,
		}

		return newTracksMsg{tracks: []SimplifiedTrack{track}}
	}
}

func syncRecentlyPlayed(client *http.Client) tea.Cmd {
	return func() tea.Msg {
		var allNewTracks []SimplifiedTrack

		url := "https://api.spotify.com/v1/me/player/recently-played?limit=50"

		for url != "" {
			resp, err := client.Get(url)

			if err != nil {
				return errorMsg{fmt.Errorf("error fetching recently played: %w", err)}
			}

			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				return errorMsg{fmt.Errorf("error status from recently-played endpoint")}
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
				Next string `json:"next"`
			}

			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				resp.Body.Close()
				return errorMsg{fmt.Errorf("error decoding recently played: %w", err)}
			}

			resp.Body.Close()

			for i := len(result.Items) - 1; i >= 0; i-- {
				item := result.Items[i].Track
				track := SimplifiedTrack{
					ID:     item.ID,
					Name:   item.Name,
					Artist: getArtistsString(item.Artists),
					URI:    item.URI,
				}

				allNewTracks = append(allNewTracks, track)
			}

			url = result.Next
		}

		if len(allNewTracks) > 0 {
			return newTracksMsg{tracks: allNewTracks}
		}

		return logMsg("Periodic sync ran, 0 new tracks found.")
	}
}

func addTracksToPlaylist(client *http.Client, playlistID string, tracks []SimplifiedTrack) tea.Cmd {
	return func() tea.Msg {
		if len(tracks) == 0 {
			return nil
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
			req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)

			if err != nil {
				return errorMsg{fmt.Errorf("error adding tracks to playlist: %w", err)}
			}

			resp.Body.Close()

			if resp.StatusCode != http.StatusCreated {
				return errorMsg{fmt.Errorf("failed to add tracks, status: %s", resp.Status)}
			}
		}

		return tracksAddedMsg{count: len(tracks)}
	}
}

func saveHistory(history map[string]SimplifiedTrack) tea.Cmd {
	return func() tea.Msg {
		file, err := os.Create(historyFileName)

		if err != nil {
			return errorMsg{fmt.Errorf("error creating history file: %w", err)}
		}

		defer file.Close()
		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")

		if err := encoder.Encode(history); err != nil {
			return errorMsg{fmt.Errorf("error encoding history to file: %w", err)}
		}

		return nil
	}
}

// --- Helper ---
func getClient(ctx context.Context, conf *oauth2.Config) (*http.Client, error) {
	tokenFile := "spotify_token.json"
	tok, err := tokenFromFile(tokenFile)

	if err == nil {
		fmt.Println("Using cached token.")
		tokenSource := conf.TokenSource(ctx, tok)
		newToken, err := tokenSource.Token()

		if err != nil {
			return nil, fmt.Errorf("could not refresh token: %w", err)
		}

		if newToken.AccessToken != tok.AccessToken {
			fmt.Println("Token was refreshed.")
			saveToken(tokenFile, newToken)
		}

		return oauth2.NewClient(ctx, tokenSource), nil
	}

	fmt.Println("Getting new token.")
	ch := make(chan *oauth2.Token)
	errCh := make(chan error)
	server := &http.Server{Addr: ":8888"}

	http.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		exchangeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		tok, err := conf.Exchange(exchangeCtx, code)

		if err != nil {
			errCh <- fmt.Errorf("could not get token: %w", err)
			return
		}

		fmt.Fprint(w, "Authentication successful! You can close this window.")
		go server.Shutdown(context.Background())
		ch <- tok
	})

	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			errCh <- fmt.Errorf("ListenAndServe(): %w", err)
		}
	}()

	url := conf.AuthCodeURL("state", oauth2.AccessTypeOffline)
	fmt.Printf("Your browser should open for Spotify authentication. If not, please visit:\n%v\n", url)
	openBrowser(url)
	select {
	case tok := <-ch:
		saveToken(tokenFile, tok)
		return conf.Client(ctx, tok), nil
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func getUserID(ctx context.Context, client *http.Client) (string, error) {
	resp, err := client.Get("https://api.spotify.com/v1/me")

	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to get user profile: %s", resp.Status)
	}

	var profile UserProfile

	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return "", err
	}

	return profile.ID, nil
}

func loadHistory() map[string]SimplifiedTrack {
	history := make(map[string]SimplifiedTrack)
	file, err := os.Open(historyFileName)

	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("History file not found, will create a new one.")
			return history
		}

		log.Fatalf("Error opening history file: %v", err)
	}

	defer file.Close()

	if err := json.NewDecoder(file).Decode(&history); err != nil {
		fmt.Printf("Could not decode history file, starting fresh: %v\n", err)
		return make(map[string]SimplifiedTrack)
	}

	return history
}
func getOrCreatePlaylist(ctx context.Context, client *http.Client, userID, name string) (string, error) {
	url := "https://api.spotify.com/v1/me/playlists?limit=50"

	for url != "" {
		resp, err := client.Get(url)

		if err != nil {
			return "", err
		}

		var playlists PagingObject

		if err := json.NewDecoder(resp.Body).Decode(&playlists); err != nil {
			resp.Body.Close()
			return "", err
		}

		resp.Body.Close() // close body

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

func getArtistsString(artists []Artist) string {
	var names []string
	for _, artist := range artists {
		names = append(names, artist.Name)
	}

	return strings.Join(names, ", ")
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
