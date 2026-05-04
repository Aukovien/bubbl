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
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/spotify"
)

// Constants
const (
	redirectURI         = "http://127.0.0.1:8888/callback"
	historyFileName     = "spotify_history.json"
	historyPlaylistName = "History"
	configFile          = "config.json"
	monitorInterval     = 30 * time.Second
	syncInterval        = 30 * time.Minute
	maxLogLines         = 50 // internal buffer; display cap is dynamic
)

// Styles
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("205")).
			MarginBottom(1)

	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("244"))

	logHeaderStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("241")).
			MarginTop(1)

	logLineStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Bold(true)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("238")).
			MarginTop(1)
)

// Structs
type Config struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

type UserProfile struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
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

// Bubble Tea Model
type model struct {
	config      *oauth2.Config
	client      *http.Client
	userID      string
	displayName string
	playlistID  string
	history     map[string]SimplifiedTrack
	spinner     spinner.Model
	status      string // current (possibly temporary) status line
	idleStatus  string // baseline status to return to after an operation
	logs        []string
	err         error
	quitting    bool
	width       int
	height      int
}

// Bubble Tea Messages
type setupDoneMsg struct {
	client      *http.Client
	userID      string
	displayName string
	playlistID  string
	history     map[string]SimplifiedTrack
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

// Main
func main() {
	m := initialModel()
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Alas, there's been an error: %v\n", err)
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
		logs:    make([]string, 0, maxLogLines),
	}
}

// Tea Init
func (m model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		doInitialSetup,
	)
}

// Event loop
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if errMsg, ok := msg.(errorMsg); ok {
		m.err = errMsg.err
		m.status = "A critical error occurred. Press 'q' to quit."
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		}
		return m, nil

	case setupDoneMsg:
		m.client = msg.client
		m.userID = msg.userID
		m.displayName = msg.displayName
		m.playlistID = msg.playlistID
		name := m.displayName
		if name == "" {
			name = m.userID
		}
		idle := fmt.Sprintf("Monitoring '%s' → playlist '%s'", name, historyPlaylistName)
		m.status = idle
		m.idleStatus = idle
		m.history = msg.history
		m.addLog(fmt.Sprintf("Loaded %d tracks from local cache.", len(m.history)))
		m.addLog("Performing initial sync of recently played songs...")
		m.status = "Syncing recently played..."
		return m, tea.Batch(
			monitorTick(),
			syncTick(),
			syncRecentlyPlayed(m.client),
		)

	case monitorTickMsg:
		cmds := []tea.Cmd{monitorTick()}
		if m.client != nil {
			cmds = append(cmds, checkCurrentlyPlaying(m.client))
		}
		return m, tea.Batch(cmds...)

	case syncTickMsg:
		cmds := []tea.Cmd{syncTick()}
		if m.client != nil {
			m.status = "Syncing recently played..."
			cmds = append(cmds, syncRecentlyPlayed(m.client))
		}
		return m, tea.Batch(cmds...)

	case newTracksMsg:
		var newTracks []SimplifiedTrack
		for _, track := range msg.tracks {
			if _, exists := m.history[track.ID]; !exists {
				m.history[track.ID] = track
				newTracks = append(newTracks, track)
				m.addLog(fmt.Sprintf("New track: %s - %s", track.Artist, track.Name))
			}
		}
		if len(newTracks) > 0 {
			m.status = fmt.Sprintf("Adding %d track(s) to playlist...", len(newTracks))
			return m, tea.Batch(
				addTracksToPlaylist(m.client, m.playlistID, newTracks),
				saveHistory(m.history),
			)
		}
		m.status = m.idleStatus
		return m, nil

	case tracksAddedMsg:
		m.addLog(fmt.Sprintf("Successfully added %d track(s) to playlist.", msg.count))
		m.status = m.idleStatus
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
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), s)
	m.logs = append(m.logs, line)
	if len(m.logs) > maxLogLines {
		m.logs = m.logs[len(m.logs)-maxLogLines:]
	}
}

// visibleLogCount returns how many log lines fit in the current terminal.
func (m model) visibleLogCount() int {
	reserved := 9 // title, status, header, help, margins
	available := m.height - reserved
	if available < 3 {
		return 3
	}
	if available > maxLogLines {
		return maxLogLines
	}
	return available
}

// Render
func (m model) View() string {
	if m.quitting {
		return "Shutting down...\n"
	}

	if m.err != nil {
		return fmt.Sprintf("\n%s\n\n%s\n\n%s\n",
			errorStyle.Render("Error: "+m.status),
			m.err.Error(),
			helpStyle.Render("Press 'q' to quit."),
		)
	}

	var b strings.Builder

	b.WriteString(titleStyle.Render("bubbl - Spotify History Monitor"))
	b.WriteString("\n")

	if m.client == nil {
		b.WriteString(m.spinner.View() + " " + statusStyle.Render(m.status) + "\n")
	} else {
		b.WriteString(statusStyle.Render(m.status) + "\n")
	}

	b.WriteString(logHeaderStyle.Render(" Activity Log "))
	b.WriteString("\n")

	visible := m.visibleLogCount()
	start := 0
	if len(m.logs) > visible {
		start = len(m.logs) - visible
	}
	for _, line := range m.logs[start:] {
		b.WriteString(logLineStyle.Render(line) + "\n")
	}

	b.WriteString(helpStyle.Render("Press 'q' to quit."))
	return b.String()
}

//  Commands

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

	userID, displayName, err := getUserID(ctx, client)
	if err != nil {
		return errorMsg{fmt.Errorf("could not get user ID: %w", err)}
	}

	history, err := loadHistory()
	if err != nil {
		return errorMsg{fmt.Errorf("could not load history: %w", err)}
	}

	playlistID, err := getOrCreatePlaylist(ctx, client, userID, historyPlaylistName)
	if err != nil {
		return errorMsg{fmt.Errorf("could not get/create playlist: %w", err)}
	}

	history, newTracksFound, err := syncPlaylistToHistory(ctx, client, playlistID, history)
	if err != nil {
		return errorMsg{fmt.Errorf("could not sync with playlist: %w", err)}
	}

	if newTracksFound > 0 {
		if err := writeHistory(history); err != nil {
			return errorMsg{fmt.Errorf("could not save synced history: %w", err)}
		}
	}

	return setupDoneMsg{
		client:      client,
		userID:      userID,
		displayName: displayName,
		playlistID:  playlistID,
		history:     history,
	}
}

func checkCurrentlyPlaying(client *http.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"https://api.spotify.com/v1/me/player/currently-playing", nil)
		if err != nil {
			return logMsg(fmt.Sprintf("Error building currently-playing request: %v", err))
		}

		resp, err := client.Do(req)
		if err != nil {
			return logMsg(fmt.Sprintf("Error checking currently playing: %v", err))
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		if resp.StatusCode != http.StatusOK {
			return logMsg(fmt.Sprintf("Unexpected status from currently-playing: %s", resp.Status))
		}

		var current CurrentlyPlaying
		if err := json.NewDecoder(resp.Body).Decode(&current); err != nil {
			return logMsg(fmt.Sprintf("Error decoding currently playing: %v", err))
		}

		if !current.IsPlaying || current.Item.ID == "" {
			return nil
		}

		return newTracksMsg{tracks: []SimplifiedTrack{{
			ID:     current.Item.ID,
			Name:   current.Item.Name,
			Artist: getArtistsString(current.Item.Artists),
			URI:    current.Item.URI,
		}}}
	}
}

func syncRecentlyPlayed(client *http.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		var allTracks []SimplifiedTrack
		url := "https://api.spotify.com/v1/me/player/recently-played?limit=50"

		for url != "" {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return errorMsg{fmt.Errorf("error building recently-played request: %w", err)}
			}

			resp, err := client.Do(req)
			if err != nil {
				return errorMsg{fmt.Errorf("error fetching recently played: %w", err)}
			}

			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				return errorMsg{fmt.Errorf("unexpected status from recently-played: %s", resp.Status)}
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

			err = json.NewDecoder(resp.Body).Decode(&result)
			resp.Body.Close()
			if err != nil {
				return errorMsg{fmt.Errorf("error decoding recently played: %w", err)}
			}

			// Append oldest-first so history is in chronological order
			for i := len(result.Items) - 1; i >= 0; i-- {
				item := result.Items[i].Track
				allTracks = append(allTracks, SimplifiedTrack{
					ID:     item.ID,
					Name:   item.Name,
					Artist: getArtistsString(item.Artists),
					URI:    item.URI,
				})
			}

			url = result.Next
		}

		if len(allTracks) > 0 {
			return newTracksMsg{tracks: allTracks}
		}
		return logMsg("Periodic sync: 0 new tracks found.")
	}
}

func addTracksToPlaylist(client *http.Client, playlistID string, tracks []SimplifiedTrack) tea.Cmd {
	return func() tea.Msg {
		if len(tracks) == 0 {
			return nil
		}

		url := fmt.Sprintf("https://api.spotify.com/v1/playlists/%s/tracks", playlistID)
		total := 0

		for i := 0; i < len(tracks); i += 100 {
			end := i + 100
			if end > len(tracks) {
				end = len(tracks)
			}
			batch := tracks[i:end]

			uris := make([]string, len(batch))
			for j, t := range batch {
				uris[j] = t.URI
			}

			body, err := json.Marshal(map[string][]string{"uris": uris})
			if err != nil {
				return errorMsg{fmt.Errorf("error marshalling track URIs: %w", err)}
			}

			if err := func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()

				req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
				if err != nil {
					return fmt.Errorf("error building add-tracks request: %w", err)
				}
				req.Header.Set("Content-Type", "application/json")

				resp, err := client.Do(req)
				if err != nil {
					return fmt.Errorf("error adding tracks to playlist: %w", err)
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusCreated {
					return fmt.Errorf("failed to add tracks, status: %s", resp.Status)
				}
				return nil
			}(); err != nil {
				return errorMsg{err}
			}

			total += len(batch)
		}

		return tracksAddedMsg{count: total}
	}
}

// saveHistory is a tea.Cmd wrapper around writeHistory.
func saveHistory(history map[string]SimplifiedTrack) tea.Cmd {
	return func() tea.Msg {
		if err := writeHistory(history); err != nil {
			return errorMsg{err}
		}
		return nil
	}
}

// writeHistory writes history to disk atomically via a temp file + rename,
// preventing data loss if the process is killed mid-write.
func writeHistory(history map[string]SimplifiedTrack) error {
	dir := filepath.Dir(historyFileName)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".history-*.json.tmp")
	if err != nil {
		return fmt.Errorf("error creating temp history file: %w", err)
	}
	tmpName := tmp.Name()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(history); encErr != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("error encoding history: %w", encErr)
	}

	if closeErr := tmp.Close(); closeErr != nil {
		os.Remove(tmpName)
		return fmt.Errorf("error closing temp file: %w", closeErr)
	}

	if renErr := os.Rename(tmpName, historyFileName); renErr != nil {
		os.Remove(tmpName)
		return fmt.Errorf("error atomically saving history file: %w", renErr)
	}

	return nil
}

//  Spotify API helpers

func syncPlaylistToHistory(ctx context.Context, client *http.Client, playlistID string, history map[string]SimplifiedTrack) (map[string]SimplifiedTrack, int, error) {
	url := fmt.Sprintf("https://api.spotify.com/v1/playlists/%s/tracks?limit=50", playlistID)
	newTracksFound := 0

	for url != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return history, newTracksFound, fmt.Errorf("error building playlist tracks request: %w", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return history, newTracksFound, err
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return history, newTracksFound, fmt.Errorf("failed to get playlist tracks: %s", resp.Status)
		}

		var page PlaylistTracksPaging
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return history, newTracksFound, err
		}

		for _, item := range page.Items {
			if item.Track.ID == "" {
				continue // skip local/podcast tracks with no Spotify ID
			}
			if _, exists := history[item.Track.ID]; !exists {
				history[item.Track.ID] = SimplifiedTrack{
					ID:     item.Track.ID,
					Name:   item.Track.Name,
					Artist: getArtistsString(item.Track.Artists),
					URI:    item.Track.URI,
				}
				newTracksFound++
			}
		}

		url = page.Next
	}

	return history, newTracksFound, nil
}

func getClient(ctx context.Context, conf *oauth2.Config) (*http.Client, error) {
	tokenFile := "spotify_token.json"

	if tok, err := tokenFromFile(tokenFile); err == nil {
		tokenSource := conf.TokenSource(ctx, tok)
		newToken, err := tokenSource.Token()
		if err != nil {
			return nil, fmt.Errorf("could not refresh token: %w", err)
		}
		if newToken.AccessToken != tok.AccessToken {
			saveToken(tokenFile, newToken)
		}
		return oauth2.NewClient(ctx, tokenSource), nil
	}

	// OAuth flow: use a dedicated mux to avoid conflicts with the default ServeMux.
	ch := make(chan *oauth2.Token, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	server := &http.Server{Addr: ":8888", Handler: mux}

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		exchangeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		tok, err := conf.Exchange(exchangeCtx, code)
		if err != nil {
			errCh <- fmt.Errorf("could not exchange token: %w", err)
			http.Error(w, "Authentication failed. You may close this window.", http.StatusInternalServerError)
			return
		}

		// Write and flush the response BEFORE blocking on the channel send,
		// so the browser gets the success page regardless of shutdown timing.
		fmt.Fprint(w, "Authentication successful! You can close this window.")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		ch <- tok // blocks until caller receives; handler returns cleanly after
	})

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("OAuth server error: %w", err)
		}
	}()

	authURL := conf.AuthCodeURL("state", oauth2.AccessTypeOffline)
	fmt.Printf("Opening browser for Spotify login. If it doesn't open, visit:\n%s\n", authURL)
	openBrowser(authURL)

	select {
	case tok := <-ch:
		// Shut down the server after receiving the token (not inside the handler),
		// so the handler has already returned and the response is fully flushed.
		go server.Shutdown(context.Background()) //nolint:errcheck
		saveToken(tokenFile, tok)
		return conf.Client(ctx, tok), nil
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func getUserID(ctx context.Context, client *http.Client) (id, displayName string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.spotify.com/v1/me", nil)
	if err != nil {
		return "", "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("failed to get user profile: %s", resp.Status)
	}

	var profile UserProfile
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return "", "", err
	}

	return profile.ID, profile.DisplayName, nil
}

// loadHistory reads the local history cache. Returns an empty map (no error)
// if the file simply doesn't exist yet, and a real error for anything else.
func loadHistory() (map[string]SimplifiedTrack, error) {
	history := make(map[string]SimplifiedTrack)

	file, err := os.Open(historyFileName)
	if err != nil {
		if os.IsNotExist(err) {
			return history, nil
		}
		return nil, fmt.Errorf("error opening history file: %w", err)
	}
	defer file.Close()

	if err := json.NewDecoder(file).Decode(&history); err != nil {
		// Corrupted cache: warn and start fresh rather than crashing.
		fmt.Fprintf(os.Stderr, "Warning: could not decode history file, starting fresh: %v\n", err)
		return make(map[string]SimplifiedTrack), nil
	}

	return history, nil
}

func getOrCreatePlaylist(ctx context.Context, client *http.Client, userID, name string) (string, error) {
	url := "https://api.spotify.com/v1/me/playlists?limit=50"

	for url != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", fmt.Errorf("error building playlists request: %w", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", fmt.Errorf("failed to list playlists: %s", resp.Status)
		}

		var playlists PagingObject
		err = json.NewDecoder(resp.Body).Decode(&playlists)
		resp.Body.Close()
		if err != nil {
			return "", err
		}

		for _, item := range playlists.Items {
			var p Playlist
			if err := json.Unmarshal(item, &p); err == nil && p.Name == name {
				return p.ID, nil
			}
		}

		url = playlists.Next
	}

	// Not found — create it.
	payload, err := json.Marshal(map[string]any{
		"name":        name,
		"public":      false,
		"description": "Listening history logged by bubbl.",
	})
	if err != nil {
		return "", fmt.Errorf("error marshalling playlist payload: %w", err)
	}

	createURL := fmt.Sprintf("https://api.spotify.com/v1/users/%s/playlists", userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, createURL, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("error building create-playlist request: %w", err)
	}
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
	if err := json.NewDecoder(file).Decode(&config); err != nil {
		return nil, err
	}

	if config.ClientID == "" || config.ClientSecret == "" {
		return nil, fmt.Errorf("config.json is missing client_id or client_secret")
	}

	return &config, nil
}

//  Utility helpers

func getArtistsString(artists []Artist) string {
	names := make([]string, len(artists))
	for i, a := range artists {
		names[i] = a.Name
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
	return tok, json.NewDecoder(f).Decode(tok)
}

func saveToken(path string, token *oauth2.Token) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Printf("Unable to cache oauth token: %v", err)
		return
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(token); err != nil {
		log.Printf("Unable to write oauth token: %v", err)
	}
}

func openBrowser(url string) {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "linux":
		cmd, args = "xdg-open", []string{url}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		cmd, args = "open", []string{url}
	default:
		log.Printf("Unsupported platform; open this URL manually: %s", url)
		return
	}

	if err := exec.Command(cmd, args...).Start(); err != nil {
		log.Printf("Could not open browser: %v", err)
	}
}

// init: interactive config bootstrapping
func init() {
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		fmt.Println("config.json not found.")
		fmt.Println("Please enter your Spotify App credentials.")

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

		enc := json.NewEncoder(file)
		enc.SetIndent("", "  ")
		if err := enc.Encode(config); err != nil {
			log.Fatalf("Unable to write config.json: %v", err)
		}

		fmt.Println("config.json created successfully.")
	}
}
