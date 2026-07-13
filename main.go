package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/spotify"
)

const (
	appName = "bubbl"

	redirectURI  = "http://127.0.0.1:8888/callback"
	callbackAddr = "127.0.0.1:8888"

	configFile      = "config.json"
	tokenFileName   = "spotify_token.json"
	historyFileName = "spotify_history.json"

	monitorInterval = 30 * time.Second
	syncInterval    = 30 * time.Minute
	authTimeout     = 5 * time.Minute
	maxLogLines     = 200

	defaultPlaylist = "History"
)

var version = "dev"

var (
	accentColor = lipgloss.Color("205")
	textColor   = lipgloss.Color("252")
	mutedColor  = lipgloss.Color("245")
	dimColor    = lipgloss.Color("241")
	faintColor  = lipgloss.Color("238")
	addColor    = lipgloss.Color("78")
	warnColor   = lipgloss.Color("214")
	errColor    = lipgloss.Color("203")
)

var (
	docStyle      = lipgloss.NewStyle().Padding(1, 2)
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(accentColor)
	versionStyle  = lipgloss.NewStyle().Foreground(faintColor)
	eyebrowStyle  = lipgloss.NewStyle().Foreground(dimColor)
	noteStyle     = lipgloss.NewStyle().Foreground(accentColor)
	trackStyle    = lipgloss.NewStyle().Foreground(textColor)
	artistStyle   = lipgloss.NewStyle().Foreground(mutedColor)
	metaStyle     = lipgloss.NewStyle().Foreground(dimColor)
	stampStyle    = lipgloss.NewStyle().Foreground(faintColor)
	helpStyle     = lipgloss.NewStyle().Foreground(faintColor)
	keyStyle      = lipgloss.NewStyle().Foreground(mutedColor)
	addStyle      = lipgloss.NewStyle().Foreground(addColor)
	warnStyle     = lipgloss.NewStyle().Foreground(warnColor)
	errStyle      = lipgloss.NewStyle().Foreground(errColor)
	errTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(errColor)
	promptStyle   = lipgloss.NewStyle().Foreground(accentColor)
)

type Config struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	PlaylistName string `json:"playlist_name"`
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

type logKind int

const (
	logInfo logKind = iota
	logAdd
	logWarn
	logErr
)

type logLine struct {
	at   time.Time
	kind logKind
	text string
}

/*
 * render - draw one line of the activity log
 *
 * the glyph carries the kind, so the eye can skip what it does not need:
 * › routine, + a track was saved, ! bubbl will retry, × bubbl stopped.
 * the prefix is a fixed thirteen columns (stamp, gap, glyph, gap) and the
 * message is truncated against that, so every line stays in one column.
 */
func (l logLine) render(width int) string {
	glyph, style := "›", metaStyle
	switch l.kind {
	case logAdd:
		glyph, style = "+", addStyle
	case logWarn:
		glyph, style = "!", warnStyle
	case logErr:
		glyph, style = "×", errStyle
	}
	const prefix = 13
	return stampStyle.Render(l.at.Format("15:04:05")) + "  " +
		style.Render(glyph) + "  " + style.Render(trunc(l.text, width-prefix))
}

type fatalError struct {
	what string
	err  error
	fix  string
}

type model struct {
	client *http.Client

	userID       string
	displayName  string
	playlistID   string
	playlistName string

	history   map[string]SimplifiedTrack
	nowTrack  SimplifiedTrack
	isPlaying bool
	lastSeen  string

	added int
	dupes int

	lastSync  time.Time
	nextSync  time.Time
	syncing   bool
	syncDupes int

	spinner  spinner.Model
	status   string
	logs     []logLine
	fatal    *fatalError
	ready    bool
	quitting bool
	width    int
	height   int
}

type addSource int

const (
	srcLive addSource = iota
	srcSync
)

type (
	connectedMsg struct {
		client *http.Client
		note   string
	}
	needAuthMsg struct {
		conf    *oauth2.Config
		state   string
		authURL string
	}
	profileMsg struct {
		id   string
		name string
	}
	playlistMsg struct {
		id      string
		created bool
	}
	cacheMsg struct {
		history  map[string]SimplifiedTrack
		fromDisk int
		fromList int
		corrupt  bool
		warn     string
	}
	nowPlayingMsg struct {
		track   SimplifiedTrack
		playing bool
	}
	syncResultMsg struct {
		tracks []SimplifiedTrack
		fatal  *fatalError
		warn   string
	}
	addResultMsg struct {
		source addSource
		tracks []SimplifiedTrack
		fatal  *fatalError
		warn   string
	}
	infoMsg        string
	warnMsg        string
	fatalMsg       fatalError
	monitorTickMsg struct{}
	syncTickMsg    struct{}
)

func monitorTick() tea.Cmd {
	return tea.Tick(monitorInterval, func(time.Time) tea.Msg { return monitorTickMsg{} })
}

func syncTick() tea.Cmd {
	return tea.Tick(syncInterval, func(time.Time) tea.Msg { return syncTickMsg{} })
}

func main() {
	setPlaylist := flag.String("set-playlist", "", "change the playlist bubbl saves to")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", appName, version)
		return
	}

	cfg, err := ensureConfig()
	if errors.Is(err, errCancelled) {
		return
	}
	if err != nil {
		die(fatalError{
			what: "bubbl can't read config.json",
			err:  err,
			fix:  "delete config.json and run bubbl again to re-enter your Client ID and Secret.",
		})
	}

	if *setPlaylist != "" {
		cfg.PlaylistName = strings.TrimSpace(*setPlaylist)
		if err := saveConfig(cfg); err != nil {
			die(fatalError{what: "bubbl couldn't save config.json", err: err,
				fix: "check that you can write to this folder, then try again."})
		}
		fmt.Printf("bubbl will save to %q from now on.\n", cfg.PlaylistName)
		return
	}

	if cfg.PlaylistName == "" {
		name, ok := askPlaylist()
		if !ok {
			return
		}
		cfg.PlaylistName = name
		if err := saveConfig(cfg); err != nil {
			die(fatalError{what: "bubbl couldn't save config.json", err: err,
				fix: "check that you can write to this folder, then try again."})
		}
	}

	p := tea.NewProgram(initialModel(cfg.PlaylistName), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		die(fatalError{what: "bubbl couldn't start", err: err, fix: "try running it from a normal terminal window."})
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `%s - a listening-history logger for Spotify

  bubbl                          watch what you play, save it, skip repeats
  bubbl --set-playlist "Name"    change the playlist bubbl saves to
  bubbl --version                print the version

bubbl keeps three files in the folder you run it from:

  config.json           your Spotify app credentials and playlist name
  spotify_token.json    your login, so you only sign in once
  spotify_history.json  every track bubbl has seen, so nothing is saved twice

None of them leave your machine.
`, appName)
}

func die(f fatalError) {
	fmt.Fprintln(os.Stderr, errTitleStyle.Render(appName+" · "+f.what))
	if f.err != nil {
		fmt.Fprintln(os.Stderr, metaStyle.Render("  "+f.err.Error()))
	}
	if f.fix != "" {
		fmt.Fprintln(os.Stderr, artistStyle.Render("  → "+f.fix))
	}
	os.Exit(1)
}

var errCancelled = errors.New("cancelled")

func ensureConfig() (*Config, error) {
	if _, err := os.Stat(configFile); errors.Is(err, os.ErrNotExist) {
		cfg, ok := askCredentials()
		if !ok {
			return nil, errCancelled
		}
		if err := saveConfig(cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	return loadConfig()
}

type credsModel struct {
	fields []textinput.Model
	focus  int
	hint   string
	done   bool
	quit   bool
}

/*
 * newCredsModel - the first run credentials form
 *
 * the secret is masked while it is typed. it would otherwise sit in the
 * scrollback of every terminal bubbl was ever set up in.
 */
func newCredsModel() credsModel {
	id := textinput.New()
	id.Prompt = "› "
	id.PromptStyle = promptStyle
	id.Placeholder = "client id"
	id.CharLimit = 128
	id.Width = 44
	id.Focus()

	secret := textinput.New()
	secret.Prompt = "› "
	secret.PromptStyle = promptStyle
	secret.Placeholder = "client secret"
	secret.CharLimit = 128
	secret.Width = 44
	secret.EchoMode = textinput.EchoPassword
	secret.EchoCharacter = '•'

	return credsModel{fields: []textinput.Model{id, secret}}
}

func (m credsModel) Init() tea.Cmd { return textinput.Blink }

func (m credsModel) valid() bool {
	for _, f := range m.fields {
		if strings.TrimSpace(f.Value()) == "" {
			return false
		}
	}
	return true
}

func (m credsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "ctrl+c", "esc":
			m.quit = true
			return m, tea.Quit

		case "enter", "tab", "shift+tab", "up", "down":
			s := key.String()
			if s == "enter" && m.focus == len(m.fields)-1 {
				if !m.valid() {
					m.hint = "bubbl needs both of these to talk to Spotify"
					return m, nil
				}
				m.done = true
				return m, tea.Quit
			}

			if s == "up" || s == "shift+tab" {
				m.focus--
			} else {
				m.focus++
			}
			if m.focus >= len(m.fields) {
				m.focus = 0
			}
			if m.focus < 0 {
				m.focus = len(m.fields) - 1
			}

			cmds := make([]tea.Cmd, len(m.fields))
			for i := range m.fields {
				if i == m.focus {
					cmds[i] = m.fields[i].Focus()
					continue
				}
				m.fields[i].Blur()
			}
			return m, tea.Batch(cmds...)
		}
	}

	cmds := make([]tea.Cmd, len(m.fields))
	for i := range m.fields {
		m.fields[i], cmds[i] = m.fields[i].Update(msg)
	}
	return m, tea.Batch(cmds...)
}

func (m credsModel) View() string {
	if m.done || m.quit {
		return ""
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render(appName) + versionStyle.Render("  first run") + "\n\n")
	b.WriteString(artistStyle.Render("Paste the credentials from your Spotify app.") + "\n")
	b.WriteString(metaStyle.Render("developer.spotify.com/dashboard → your app → settings") + "\n\n")

	b.WriteString(eyebrowStyle.Render("CLIENT ID") + "\n" + m.fields[0].View() + "\n\n")
	b.WriteString(eyebrowStyle.Render("CLIENT SECRET") + "\n" + m.fields[1].View() + "\n\n")

	if m.hint != "" {
		b.WriteString(warnStyle.Render("! "+m.hint) + "\n\n")
	}

	b.WriteString(hint("tab", "next") + metaStyle.Render(" · ") + hint("enter", "save") + metaStyle.Render(" · ") + hint("ctrl+c", "cancel") + "\n")
	b.WriteString(metaStyle.Render("both are written to ./config.json and stay on this machine"))
	return docStyle.Render(b.String())
}

func askCredentials() (*Config, bool) {
	res, err := tea.NewProgram(newCredsModel()).Run()
	if err != nil {
		die(fatalError{what: "bubbl couldn't ask for your credentials", err: err,
			fix: "try running bubbl from a normal terminal window."})
	}
	final, ok := res.(credsModel)
	if !ok || !final.done {
		return nil, false
	}
	return &Config{
		ClientID:     strings.TrimSpace(final.fields[0].Value()),
		ClientSecret: strings.TrimSpace(final.fields[1].Value()),
	}, true
}

type promptModel struct {
	input textinput.Model
	done  bool
	quit  bool
}

func newPromptModel() promptModel {
	ti := textinput.New()
	ti.Prompt = "› "
	ti.PromptStyle = promptStyle
	ti.Placeholder = defaultPlaylist
	ti.CharLimit = 100
	ti.Width = 44
	ti.Focus()
	return promptModel{input: ti}
}

func (m promptModel) Init() tea.Cmd { return textinput.Blink }

func (m promptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "ctrl+c", "esc":
			m.quit = true
			return m, tea.Quit
		case "enter":
			m.done = true
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m promptModel) View() string {
	if m.done || m.quit {
		return ""
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(appName) + "\n\n")
	b.WriteString(artistStyle.Render("Where should bubbl save your listening history?") + "\n")
	b.WriteString(metaStyle.Render("A private playlist. bubbl makes it if it doesn't exist yet.") + "\n\n")
	b.WriteString(m.input.View() + "\n\n")
	b.WriteString(hint("enter", "save") + metaStyle.Render(" · ") + hint("ctrl+c", "cancel"))
	return docStyle.Render(b.String())
}

func askPlaylist() (string, bool) {
	res, err := tea.NewProgram(newPromptModel()).Run()
	if err != nil {
		die(fatalError{what: "bubbl couldn't ask which playlist to use", err: err,
			fix: "set it directly instead:  bubbl --set-playlist \"History\""})
	}
	final, ok := res.(promptModel)
	if !ok || !final.done {
		return "", false
	}
	name := strings.TrimSpace(final.input.Value())
	if name == "" {
		name = defaultPlaylist
	}
	return name, true
}

func initialModel(playlistName string) model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = noteStyle

	m := model{
		spinner:      s,
		status:       "connecting to Spotify...",
		playlistName: playlistName,
		history:      make(map[string]SimplifiedTrack),
		logs:         make([]logLine, 0, maxLogLines),
	}
	m.addLog(logInfo, "connecting to Spotify...")
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, connect())
}

func (m *model) addLog(kind logKind, text string) {
	m.logs = append(m.logs, logLine{at: time.Now(), kind: kind, text: text})
	if len(m.logs) > maxLogLines {
		m.logs = m.logs[len(m.logs)-maxLogLines:]
	}
}

func (m model) stop(f fatalError) (tea.Model, tea.Cmd) {
	m.fatal = &f
	m.addLog(logErr, f.what)
	return m, nil
}

/*
 * Update - the state machine
 *
 * startup is a chain, not one blocking call: connect, then fetchProfile, then
 * ensurePlaylist, then loadCache, each logging its own line before firing the
 * next. that is why the log reads like a story instead of one long spinner,
 * and why a failure says which step it failed at.
 *
 * after that two timers drive everything. the monitor ticks every thirty
 * seconds and asks what is playing; the sync ticks every thirty minutes and
 * asks what was missed.
 */
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.quitting = true
			return m, tea.Quit

		case "s":
			if m.ready && !m.syncing && m.fatal == nil {
				m.syncing = true
				m.addLog(logInfo, "syncing missed history...")
				return m, syncRecentlyPlayed(m.client)
			}

		case "o":
			if m.ready && m.playlistID != "" {
				if err := openBrowser(playlistURL(m.playlistID)); err != nil {
					m.addLog(logWarn, "couldn't open your browser · "+playlistURL(m.playlistID))
				} else {
					m.addLog(logInfo, "opened "+quote(m.playlistName)+" in your browser")
				}
			}

		case "c":
			m.logs = m.logs[:0]
		}
		return m, nil

	case fatalMsg:
		return m.stop(fatalError(msg))

	case needAuthMsg:
		m.status = "waiting for you to approve bubbl"
		m.addLog(logInfo, "waiting for you to approve bubbl in your browser")
		m.addLog(logInfo, "didn't open? "+msg.authURL)
		return m, awaitAuth(msg.conf, msg.state, msg.authURL)

	case connectedMsg:
		m.client = msg.client
		m.status = "checking your account"
		m.addLog(logInfo, "auth successful")
		if msg.note != "" {
			m.addLog(logWarn, msg.note)
		}
		return m, fetchProfile(m.client)

	case profileMsg:
		m.userID, m.displayName = msg.id, msg.name
		m.status = "opening the playlist"
		m.addLog(logInfo, "signed in as "+m.who())
		return m, ensurePlaylist(m.client, m.userID, m.playlistName)

	case playlistMsg:
		m.playlistID = msg.id
		m.status = "reading what's already saved"
		if msg.created {
			m.addLog(logAdd, "created the playlist "+quote(m.playlistName))
		} else {
			m.addLog(logInfo, "found your playlist "+quote(m.playlistName))
		}
		return m, loadCache(m.client, m.playlistID)

	case cacheMsg:
		m.history = msg.history
		m.ready = true

		if msg.corrupt {
			m.addLog(logWarn, historyFileName+" was unreadable · bubbl rebuilt it from your playlist")
		}
		switch {
		case msg.fromDisk > 0:
			m.addLog(logInfo, fmt.Sprintf("loaded %d tracks from the local cache", msg.fromDisk))
		default:
			m.addLog(logInfo, "no local cache yet · starting one")
		}
		if msg.fromList > 0 {
			m.addLog(logInfo, fmt.Sprintf("picked up %d tracks already in the playlist", msg.fromList))
		}
		if msg.warn != "" {
			m.addLog(logWarn, msg.warn)
		}

		m.addLog(logInfo, "syncing missed history...")
		m.syncing = true
		m.nextSync = time.Now().Add(syncInterval)
		return m, tea.Batch(monitorTick(), syncTick(), syncRecentlyPlayed(m.client))

	case monitorTickMsg:
		if m.fatal != nil {
			return m, nil
		}
		cmds := []tea.Cmd{monitorTick()}
		if m.client != nil {
			cmds = append(cmds, checkCurrentlyPlaying(m.client))
		}
		return m, tea.Batch(cmds...)

	case syncTickMsg:
		if m.fatal != nil {
			return m, nil
		}
		m.nextSync = time.Now().Add(syncInterval)
		cmds := []tea.Cmd{syncTick()}
		if m.client != nil && !m.syncing {
			m.syncing = true
			m.addLog(logInfo, "syncing missed history...")
			cmds = append(cmds, syncRecentlyPlayed(m.client))
		}
		return m, tea.Batch(cmds...)

	case nowPlayingMsg:
		m.isPlaying = msg.playing
		m.nowTrack = msg.track

		if !msg.playing || msg.track.ID == "" || msg.track.ID == m.lastSeen {
			return m, nil
		}
		m.lastSeen = msg.track.ID

		if _, known := m.history[msg.track.ID]; known {
			m.dupes++
			m.addLog(logInfo, "already saved · "+trackLine(msg.track))
			return m, nil
		}
		return m, addTracks(m.client, m.playlistID, []SimplifiedTrack{msg.track}, srcLive)

	case syncResultMsg:
		m.syncing = false
		if msg.fatal != nil {
			return m.stop(*msg.fatal)
		}
		if msg.warn != "" {
			m.addLog(logWarn, msg.warn)
			return m, nil
		}

		m.lastSync = time.Now()

		seen := make(map[string]bool, len(msg.tracks))
		fresh := make([]SimplifiedTrack, 0, len(msg.tracks))
		dupes := 0
		for _, t := range msg.tracks {
			if t.ID == "" || seen[t.ID] {
				continue
			}
			seen[t.ID] = true
			if _, known := m.history[t.ID]; known {
				dupes++
				continue
			}
			fresh = append(fresh, t)
		}
		m.dupes += dupes
		m.syncDupes = dupes

		if len(fresh) == 0 {
			m.logSync(0, dupes)
			return m, nil
		}
		return m, addTracks(m.client, m.playlistID, fresh, srcSync)

	case addResultMsg:
		if msg.fatal != nil {
			return m.stop(*msg.fatal)
		}
		if msg.warn != "" {
			m.addLog(logWarn, msg.warn)

			if msg.source == srcLive {
				m.lastSeen = ""
			}
			return m, nil
		}

		for _, t := range msg.tracks {
			m.history[t.ID] = t
		}
		m.added += len(msg.tracks)

		if msg.source == srcLive {
			for _, t := range msg.tracks {
				m.addLog(logAdd, trackLine(t))
			}
		} else {
			m.logSync(len(msg.tracks), m.syncDupes)
		}
		return m, saveHistory(m.history)

	case infoMsg:
		m.addLog(logInfo, string(msg))
		return m, nil

	case warnMsg:
		m.addLog(logWarn, string(msg))
		return m, nil

	default:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
}

func (m *model) logSync(added, dupes int) {
	if added == 0 && dupes == 0 {
		m.addLog(logInfo, "sync · nothing new to save")
		return
	}
	if added > 0 {
		m.addLog(logAdd, fmt.Sprintf("%d %s added", added, plural(added, "track", "tracks")))
	}
	m.addLog(logInfo, fmt.Sprintf("duplicates found: %d", dupes))
}

func (m model) who() string {
	if m.displayName != "" {
		return m.displayName
	}
	return m.userID
}

/*
 * View - draw the screen
 *
 * the terminal is a fixed budget of rows. the title, what is playing and the
 * key hints are always drawn; what is left over goes to the log, which is then
 * padded to that exact height so the hints stay pinned to the bottom instead of
 * walking up the screen as lines arrive.
 *
 * in a short window the counters are dropped first and the log second, because
 * knowing what is playing is worth more than either.
 */
func (m model) View() string {
	if m.fatal != nil {
		return m.fatalView()
	}
	if m.quitting {
		return m.farewellView()
	}

	header := m.headerView()
	now := m.nowPlayingView()
	stats := m.statsView()
	help := m.helpView()

	height := m.height
	if height == 0 {
		height = 24
	}

	rest := height - (lipgloss.Height(header) + lipgloss.Height(now) + lipgloss.Height(help) + 4 + 2)

	body := header + "\n\n" + now

	if stats != "" && rest >= lipgloss.Height(stats) {
		body += "\n\n" + stats
		rest -= lipgloss.Height(stats)
	}

	room := rest - 1
	if room > maxLogLines {
		room = maxLogLines
	}
	if room >= 3 {
		body += "\n\n" + eyebrowStyle.Render("ACTIVITY") + "\n" + m.logView(room)
	}

	return docStyle.Render(body + "\n\n" + help)
}

func (m model) headerView() string {
	return titleStyle.Render(appName)
}

func (m model) nowPlayingView() string {
	if !m.ready {
		return m.spinner.View() + " " + artistStyle.Render(m.status)
	}

	w := m.innerWidth()

	switch {
	case m.isPlaying && m.nowTrack.ID != "":
		return trackStyle.Render(trunc(m.nowTrack.Name, w)) + "\n" +
			artistStyle.Render(trunc(m.nowTrack.Artist, w))

	case m.nowTrack.ID != "":
		return metaStyle.Render("paused") + "\n" +
			metaStyle.Render(trunc(trackLine(m.nowTrack), w))

	default:
		return metaStyle.Render("nothing playing")
	}
}

func (m model) statsView() string {
	if !m.ready {
		return ""
	}

	counts := strings.Join([]string{
		fmt.Sprintf("%d saved", len(m.history)),
		fmt.Sprintf("%d added this session", m.added),
		fmt.Sprintf("%d duplicates skipped", m.dupes),
	}, " · ")

	var sync string
	switch {
	case m.syncing:
		sync = "syncing now"
	case m.lastSync.IsZero():
		sync = "first sync on the way"
	default:
		sync = fmt.Sprintf("synced %s · next in %s", ago(m.lastSync), until(m.nextSync))
	}

	w := m.innerWidth()
	return trackStyle.Render(trunc(m.playlistName, w)) + "\n" +
		metaStyle.Render(trunc(counts, w)) + "\n" +
		metaStyle.Render(trunc(sync, w))
}

func (m model) logView(room int) string {
	if room <= 0 {
		return ""
	}

	lines := make([]string, 0, room)
	start := 0
	if len(m.logs) > room {
		start = len(m.logs) - room
	}
	for _, l := range m.logs[start:] {
		lines = append(lines, l.render(m.innerWidth()))
	}

	for len(lines) < room {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (m model) helpView() string {
	if !m.ready {
		return hint("q", "quit")
	}
	sep := metaStyle.Render(" · ")
	return hint("s", "sync now") + sep + hint("o", "open playlist") + sep +
		hint("c", "clear") + sep + hint("q", "quit")
}

func (m model) fatalView() string {
	f := m.fatal

	var b strings.Builder
	b.WriteString(titleStyle.Render(appName) + "\n\n")
	b.WriteString(errTitleStyle.Render("×  "+f.what) + "\n\n")
	if f.err != nil {
		b.WriteString("   " + metaStyle.Render(trunc(f.err.Error(), m.innerWidth()-3)) + "\n\n")
	}
	if f.fix != "" {
		b.WriteString(noteStyle.Render("→  ") + artistStyle.Render(f.fix) + "\n\n")
	}
	b.WriteString(hint("q", "quit"))
	return docStyle.Render(b.String())
}

func (m model) farewellView() string {
	if !m.ready {
		return ""
	}
	summary := "nothing new this session"
	if m.added > 0 {
		summary = fmt.Sprintf("%d %s added to %s", m.added, plural(m.added, "track", "tracks"), quote(m.playlistName))
	}
	return titleStyle.Render(appName) + metaStyle.Render(" · "+summary+" · "+
		fmt.Sprintf("%d tracks saved in total", len(m.history))) + "\n"
}

func hint(key, desc string) string {
	return keyStyle.Render(key) + " " + helpStyle.Render(desc)
}

func (m model) innerWidth() int {
	w := m.width - 4
	if w < 40 {
		return 76
	}
	return w
}

/*
 * oauthConfig - what bubbl asks spotify for permission to do
 *
 * user-read-currently-playing   see the song playing right now
 * user-read-recently-played     see what played while bubbl was closed
 * playlist-read-private         find the history playlist
 * playlist-read-collaborative   ...even when it is a shared one
 * playlist-modify-private       put tracks in it
 * playlist-modify-public        ...whichever kind it turns out to be
 *
 * nothing else is requested. bubbl cannot read the saved library, follow
 * anyone, or change what is playing.
 */
func oauthConfig(cfg *Config) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  redirectURI,
		Scopes: []string{
			"user-read-recently-played",
			"user-read-currently-playing",
			"playlist-read-private",
			"playlist-read-collaborative",
			"playlist-modify-private",
			"playlist-modify-public",
		},
		Endpoint: spotify.Endpoint,
	}
}

/*
 * connect - get an authenticated client for spotify
 *
 * a cached token is refreshed and reused, so the browser dance happens once per
 * machine and never again. with no token on disk this returns needAuthMsg
 * rather than blocking, which lets the tui put the login url on screen before a
 * browser is opened at it.
 */
func connect() tea.Cmd {
	return func() tea.Msg {
		cfg, err := loadConfig()
		if err != nil {
			return fatalMsg{
				what: "bubbl can't read config.json",
				err:  err,
				fix:  "delete config.json and run bubbl again to re-enter your Client ID and Secret.",
			}
		}
		conf := oauthConfig(cfg)

		if tok, err := tokenFromFile(tokenFileName); err == nil {
			src := conf.TokenSource(context.Background(), tok)
			fresh, err := src.Token()
			if err != nil {
				return fatalMsg{
					what: "Spotify wouldn't accept your saved login",
					err:  err,
					fix:  "delete " + tokenFileName + " and run bubbl again to sign in.",
				}
			}
			note := ""
			if fresh.AccessToken != tok.AccessToken {
				if err := saveToken(tokenFileName, fresh); err != nil {
					note = "couldn't update " + tokenFileName + " · you may have to sign in again next time"
				}
			}
			return connectedMsg{client: oauth2.NewClient(context.Background(), src), note: note}
		}

		state, err := nonce()
		if err != nil {
			return fatalMsg{what: "bubbl couldn't start a secure login", err: err,
				fix: "try again. if it keeps happening, open an issue."}
		}
		return needAuthMsg{conf: conf, state: state, authURL: conf.AuthCodeURL(state, oauth2.AccessTypeOffline)}
	}
}

/*
 * awaitAuth - hold port 8888 open for the spotify redirect
 *
 * the port is bound before the browser is opened, so "address already in use"
 * is reported as itself instead of as a login that mysteriously never finishes.
 *
 * the redirect is only accepted if its state matches the nonce this login was
 * started with. without that check any page in the browser could hand bubbl a
 * login code of its choosing.
 *
 * nothing is printed here. stdout would land in the middle of the alt screen.
 */
func awaitAuth(conf *oauth2.Config, state, authURL string) tea.Cmd {
	return func() tea.Msg {
		ln, err := net.Listen("tcp", callbackAddr)
		if err != nil {
			return fatalMsg{
				what: "port 8888 is already in use",
				err:  err,
				fix:  "bubbl listens there to catch the Spotify redirect. close whatever else is on 127.0.0.1:8888 and run bubbl again.",
			}
		}

		tokens := make(chan *oauth2.Token, 1)
		problems := make(chan error, 1)

		mux := http.NewServeMux()
		mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()

			if denied := q.Get("error"); denied != "" {
				http.Error(w, "bubbl: Spotify said "+denied+". You can close this tab.", http.StatusBadRequest)
				problems <- fmt.Errorf("Spotify returned %q", denied)
				return
			}

			if q.Get("state") != state {
				http.Error(w, "bubbl: that login didn't come from bubbl. Ignoring it.", http.StatusBadRequest)
				problems <- errors.New("the redirect came back with the wrong state")
				return
			}
			code := q.Get("code")
			if code == "" {
				http.Error(w, "bubbl: Spotify didn't send a login code. You can close this tab.", http.StatusBadRequest)
				problems <- errors.New("Spotify sent no login code")
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			tok, err := conf.Exchange(ctx, code)
			if err != nil {
				http.Error(w, "bubbl: couldn't finish the login. Check the terminal.", http.StatusInternalServerError)
				problems <- fmt.Errorf("could not trade the login code for a token: %w", err)
				return
			}

			io.WriteString(w, successPage)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			tokens <- tok
		})

		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(ln)
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			srv.Shutdown(ctx)
		}()

		openBrowser(authURL)

		select {
		case tok := <-tokens:
			note := ""
			if err := saveToken(tokenFileName, tok); err != nil {
				note = "couldn't save " + tokenFileName + " · you'll have to sign in again next run"
			}
			return connectedMsg{client: conf.Client(context.Background(), tok), note: note}

		case err := <-problems:
			return fatalMsg{
				what: "the Spotify login didn't finish",
				err:  err,
				fix:  "run bubbl again and approve it in the browser tab that opens.",
			}

		case <-time.After(authTimeout):
			return fatalMsg{
				what: "gave up waiting for the Spotify login",
				err:  fmt.Errorf("nothing came back within %s", authTimeout),
				fix:  "run bubbl again and finish the login in the browser tab it opens.",
			}
		}
	}
}

func fetchProfile(client *http.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.spotify.com/v1/me", nil)
		resp, err := client.Do(req)
		if err != nil {
			return fatalMsg{what: "bubbl couldn't reach Spotify", err: err,
				fix: "check your connection and run bubbl again."}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			if f, _ := problem(resp, "looking up your account"); f != nil {
				return fatalMsg(*f)
			}
			return fatalMsg{what: "Spotify wouldn't say who you are", err: errors.New(resp.Status),
				fix: "wait a moment and run bubbl again."}
		}

		var p UserProfile
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			return fatalMsg{what: "bubbl couldn't read your Spotify profile", err: err,
				fix: "run bubbl again."}
		}
		return profileMsg{id: p.ID, name: p.DisplayName}
	}
}

func ensurePlaylist(client *http.Client, userID, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		id, created, err := getOrCreatePlaylist(ctx, client, userID, name)
		if err != nil {
			return fatalMsg{
				what: "bubbl couldn't open the playlist " + quote(name),
				err:  err,
				fix:  "check the playlist-modify scopes on your Spotify app, or point bubbl somewhere else:  bubbl --set-playlist \"Name\"",
			}
		}
		return playlistMsg{id: id, created: created}
	}
}

/*
 * loadCache - work out what bubbl already knows
 *
 * the cache file is read first, then the playlist itself, which catches tracks
 * put there by hand or from another machine.
 *
 * a playlist that cannot be read is only fatal when the cache is empty. with a
 * cache bubbl can still tell a repeat from a new track; without one it would
 * add every song a second time.
 */
func loadCache(client *http.Client, playlistID string) tea.Cmd {
	return func() tea.Msg {
		history, corrupt := loadHistory()
		fromDisk := len(history)

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		history, fromList, err := syncPlaylistToHistory(ctx, client, playlistID, history)
		if err != nil {
			if fromDisk == 0 {
				return fatalMsg{
					what: "bubbl couldn't read the playlist",
					err:  err,
					fix:  "it needs to know what's already in there before it adds anything. check your connection and run bubbl again.",
				}
			}
			return cacheMsg{
				history:  history,
				fromDisk: fromDisk,
				corrupt:  corrupt,
				warn:     "couldn't read the playlist just now · working from the local cache",
			}
		}

		if fromList > 0 {
			writeHistory(history)
		}
		return cacheMsg{history: history, fromDisk: fromDisk, fromList: fromList, corrupt: corrupt}
	}
}

/*
 * checkCurrentlyPlaying - ask spotify what is on right now
 *
 * 204 means nothing is playing anywhere. a 200 with is_playing false means it
 * is paused, and the track is still worth showing.
 *
 * failures here are warnings, never a stop. the next tick is thirty seconds
 * away and will ask again.
 */
func checkCurrentlyPlaying(client *http.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			"https://api.spotify.com/v1/me/player/currently-playing", nil)

		resp, err := client.Do(req)
		if err != nil {
			return warnMsg("couldn't reach Spotify · trying again in 30s")
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNoContent {
			return nowPlayingMsg{playing: false}
		}
		if resp.StatusCode != http.StatusOK {
			if f, w := problem(resp, "checking what's playing"); f != nil {
				return fatalMsg(*f)
			} else {
				return warnMsg(w)
			}
		}

		var current CurrentlyPlaying
		if err := json.NewDecoder(resp.Body).Decode(&current); err != nil {
			return warnMsg("couldn't read what's playing · trying again in 30s")
		}
		if current.Item.ID == "" {
			return nowPlayingMsg{playing: false}
		}

		return nowPlayingMsg{
			playing: current.IsPlaying,
			track: SimplifiedTrack{
				ID:     current.Item.ID,
				Name:   current.Item.Name,
				Artist: getArtistsString(current.Item.Artists),
				URI:    current.Item.URI,
			},
		}
	}
}

/*
 * syncRecentlyPlayed - catch everything played while bubbl was closed
 *
 * this is the half of bubbl that makes it worth running at all. spotify keeps
 * the last fifty plays and hands them over newest first; they are reversed here
 * so the playlist ends up in the order they were actually heard.
 */
func syncRecentlyPlayed(client *http.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		var all []SimplifiedTrack
		url := "https://api.spotify.com/v1/me/player/recently-played?limit=50"

		for url != "" {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return syncResultMsg{warn: "sync failed · trying again shortly"}
			}

			resp, err := client.Do(req)
			if err != nil {
				return syncResultMsg{warn: "couldn't reach Spotify to sync · trying again later"}
			}

			if resp.StatusCode != http.StatusOK {
				f, w := problem(resp, "reading your recently played tracks")
				resp.Body.Close()
				return syncResultMsg{fatal: f, warn: w}
			}

			var page struct {
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

			err = json.NewDecoder(resp.Body).Decode(&page)
			resp.Body.Close()
			if err != nil {
				return syncResultMsg{warn: "couldn't read the recently played list · trying again later"}
			}

			for i := len(page.Items) - 1; i >= 0; i-- {
				t := page.Items[i].Track
				all = append(all, SimplifiedTrack{
					ID:     t.ID,
					Name:   t.Name,
					Artist: getArtistsString(t.Artists),
					URI:    t.URI,
				})
			}
			url = page.Next
		}

		return syncResultMsg{tracks: all}
	}
}

/*
 * addTracks - write tracks to the playlist, then report what landed
 *
 * spotify takes at most a hundred uris per request, so long batches are split.
 *
 * the local cache is deliberately left alone in here. it is written only once
 * spotify has confirmed the add, so a request that fails leaves those tracks
 * still unknown and the next tick picks them up again. the other way round, a
 * failed add would mark them saved and lose them for good.
 */
func addTracks(client *http.Client, playlistID string, tracks []SimplifiedTrack, source addSource) tea.Cmd {
	return func() tea.Msg {
		if len(tracks) == 0 {
			return nil
		}

		url := fmt.Sprintf("https://api.spotify.com/v1/playlists/%s/tracks", playlistID)

		for i := 0; i < len(tracks); i += 100 {
			end := min(i+100, len(tracks))
			batch := tracks[i:end]

			uris := make([]string, len(batch))
			for j, t := range batch {
				uris[j] = t.URI
			}
			body, err := json.Marshal(map[string][]string{"uris": uris})
			if err != nil {
				return addResultMsg{source: source, warn: "couldn't prepare those tracks · they'll be retried"}
			}

			result := func() *addResultMsg {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()

				req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
				if err != nil {
					return &addResultMsg{source: source, warn: "couldn't build the request · retrying shortly"}
				}
				req.Header.Set("Content-Type", "application/json")

				resp, err := client.Do(req)
				if err != nil {
					return &addResultMsg{source: source, warn: "couldn't reach Spotify to save · retrying shortly"}
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusCreated {
					f, w := problem(resp, "saving tracks to the playlist")
					return &addResultMsg{source: source, fatal: f, warn: w}
				}
				return nil
			}()

			if result != nil {
				return *result
			}
		}

		return addResultMsg{source: source, tracks: tracks}
	}
}

/*
 * saveHistory - write the cache to disk
 *
 * the snapshot is taken here, on the update goroutine, and not inside the
 * command that is returned. the model goes on mutating that map while the write
 * runs, and a map cannot be read and written at the same time.
 */
func saveHistory(history map[string]SimplifiedTrack) tea.Cmd {
	snapshot := make(map[string]SimplifiedTrack, len(history))
	for id, t := range history {
		snapshot[id] = t
	}
	return func() tea.Msg {
		if err := writeHistory(snapshot); err != nil {
			return warnMsg("couldn't write " + historyFileName + " · the tracks are safe in the playlist")
		}
		return nil
	}
}

/*
 * problem - decide whether a bad response stops bubbl or not
 *
 * 401 and 403 will not fix themselves, so they stop it, and each one carries
 * the single thing to do about it.
 *
 * everything else, rate limits included, is a warning. the monitor is on a
 * thirty second loop and the sync on a thirty minute one, so the retry is
 * already scheduled and there is nothing to tell the user to do.
 */
func problem(resp *http.Response, doing string) (*fatalError, string) {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return &fatalError{
			what: "Spotify stopped accepting bubbl's login",
			err:  fmt.Errorf("401 unauthorized while %s", doing),
			fix:  "delete " + tokenFileName + " and run bubbl again to sign back in.",
		}, ""

	case http.StatusForbidden:
		return &fatalError{
			what: "Spotify refused the request",
			err:  fmt.Errorf("403 forbidden while %s", doing),
			fix:  "your Spotify app is probably missing a scope. delete " + tokenFileName + " and run bubbl again to re-approve it.",
		}, ""

	case http.StatusTooManyRequests:
		wait := "a moment"
		if after := resp.Header.Get("Retry-After"); after != "" {
			wait = after + "s"
		}
		return nil, "Spotify is rate-limiting bubbl · backing off for " + wait

	default:
		return nil, fmt.Sprintf("Spotify returned %s while %s · will retry", resp.Status, doing)
	}
}

/*
 * getOrCreatePlaylist - find the playlist by name, or make it
 *
 * the match is on the name and not an id, so renaming the playlist in spotify
 * makes bubbl quietly build a fresh one under the old name. the cache still
 * remembers every track it has ever seen, so that new playlist stays empty
 * until something unheard plays. delete spotify_history.json to fill it from
 * scratch.
 */
func getOrCreatePlaylist(ctx context.Context, client *http.Client, userID, name string) (id string, created bool, err error) {
	url := "https://api.spotify.com/v1/me/playlists?limit=50"

	for url != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", false, err
		}

		resp, err := client.Do(req)
		if err != nil {
			return "", false, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", false, fmt.Errorf("couldn't list your playlists: %s", resp.Status)
		}

		var page PagingObject
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return "", false, err
		}

		for _, item := range page.Items {
			var p Playlist
			if err := json.Unmarshal(item, &p); err == nil && p.Name == name {
				return p.ID, false, nil
			}
		}
		url = page.Next
	}

	payload, err := json.Marshal(map[string]any{
		"name":        name,
		"public":      false,
		"description": "Everything I've listened to. Logged by bubbl.",
	})
	if err != nil {
		return "", false, err
	}

	createURL := fmt.Sprintf("https://api.spotify.com/v1/users/%s/playlists", userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, createURL, bytes.NewReader(payload))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", false, fmt.Errorf("couldn't create the playlist: %s", strings.TrimSpace(string(body)))
	}

	var made Playlist
	if err := json.NewDecoder(resp.Body).Decode(&made); err != nil {
		return "", false, err
	}
	return made.ID, true, nil
}

/*
 * syncPlaylistToHistory - teach bubbl about tracks it did not add itself
 *
 * the playlist, not the cache file, is the real record. anything sitting in it
 * that the cache has never seen is folded in here, which is what stops a fresh
 * checkout, a second machine, or a hand-added track from being added twice.
 */
func syncPlaylistToHistory(ctx context.Context, client *http.Client, playlistID string, history map[string]SimplifiedTrack) (map[string]SimplifiedTrack, int, error) {
	url := fmt.Sprintf("https://api.spotify.com/v1/playlists/%s/tracks?limit=50", playlistID)
	found := 0

	for url != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return history, found, err
		}

		resp, err := client.Do(req)
		if err != nil {
			return history, found, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return history, found, fmt.Errorf("couldn't read the playlist: %s", resp.Status)
		}

		var page PlaylistTracksPaging
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return history, found, err
		}

		for _, item := range page.Items {
			t := item.Track
			if t.ID == "" {
				continue
			}
			if _, exists := history[t.ID]; exists {
				continue
			}
			history[t.ID] = SimplifiedTrack{
				ID:     t.ID,
				Name:   t.Name,
				Artist: getArtistsString(t.Artists),
				URI:    t.URI,
			}
			found++
		}
		url = page.Next
	}

	return history, found, nil
}

/*
 * loadHistory - read the cache file
 *
 * this cannot fail. a cache that will not decode is a cache worth throwing
 * away, and syncPlaylistToHistory rebuilds it from the playlist moments later.
 */
func loadHistory() (history map[string]SimplifiedTrack, corrupt bool) {
	history = make(map[string]SimplifiedTrack)

	file, err := os.Open(historyFileName)
	if err != nil {
		return history, false
	}
	defer file.Close()

	if err := json.NewDecoder(file).Decode(&history); err != nil {
		return make(map[string]SimplifiedTrack), true
	}
	return history, false
}

/*
 * writeHistory - save the cache atomically
 *
 * the write goes to a temp file in the same directory and is then renamed over
 * the old one, because a rename within one filesystem either happens or does
 * not. a crash halfway through leaves the previous cache whole instead of half
 * a file of json.
 */
func writeHistory(history map[string]SimplifiedTrack) error {
	dir := filepath.Dir(historyFileName)
	if dir == "" {
		dir = "."
	}

	tmp, err := os.CreateTemp(dir, ".history-*.json.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(history); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, historyFileName); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func loadConfig() (*Config, error) {
	file, err := os.Open(configFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var cfg Config
	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		return nil, err
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("config.json has no client_id or client_secret in it")
	}
	return &cfg, nil
}

func saveConfig(cfg *Config) error {
	f, err := os.OpenFile(configFile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(cfg)
}

func tokenFromFile(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tok := &oauth2.Token{}
	if err := json.NewDecoder(f).Decode(tok); err != nil {
		return nil, err
	}
	return tok, nil
}

func saveToken(path string, token *oauth2.Token) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(token)
}

/*
 * openBrowser - open a url and say nothing about it
 *
 * failures are not logged or printed. the tui owns the screen, and anything
 * written to stdout or stderr lands in the middle of it. callers put the url on
 * screen instead, so a browser that refuses to open is never a dead end.
 */
func openBrowser(url string) error {
	var name string
	var args []string

	switch runtime.GOOS {
	case "linux":
		name, args = "xdg-open", []string{url}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		name, args = "open", []string{url}
	default:
		return fmt.Errorf("bubbl doesn't know how to open a browser on %s", runtime.GOOS)
	}

	return exec.Command(name, args...).Start()
}

func playlistURL(id string) string {
	return "https://open.spotify.com/playlist/" + id
}

func nonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func getArtistsString(artists []Artist) string {
	names := make([]string, len(artists))
	for i, a := range artists {
		names[i] = a.Name
	}
	return strings.Join(names, ", ")
}

func trackLine(t SimplifiedTrack) string {
	if t.Artist == "" {
		return t.Name
	}
	return t.Artist + " - " + t.Name
}

func quote(s string) string { return "\"" + s + "\"" }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

/*
 * trunc - cut a string to width and mark it with an ellipsis
 *
 * the ellipsis is assumed to be one cell wide. swap it for "..." and this has
 * to become width-3, or every truncated line runs two columns long and drags
 * the log out of alignment.
 */
func trunc(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 45*time.Second:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
	}
}

func until(t time.Time) string {
	d := time.Until(t)
	switch {
	case d <= 0:
		return "any moment"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes())+1)
	}
}

const successPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>bubbl · connected</title>
  <style>
    :root { color-scheme: dark; }
    body {
      margin: 0; height: 100vh; display: grid; place-items: center;
      background: #16161d; color: #e4e4e7;
      font: 400 15px/1.7 "JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, monospace;
    }
    .name { color: #ff6ac1; font-weight: 700; margin-bottom: 1.2rem; }
    .ok { color: #5fd787; margin: 0; }
    .muted { color: #71717a; margin: .4rem 0 0; }
  </style>
</head>
<body>
  <main>
    <div class="name">bubbl</div>
    <p class="ok">&rsaquo; auth successful</p>
    <p class="muted">You can close this tab. bubbl is listening now.</p>
  </main>
</body>
</html>`
