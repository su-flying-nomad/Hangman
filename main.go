package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
)

const (
	maxWrongGuesses = 8
	roomIDLength    = 6
	totalRounds     = 15
	roundDuration   = 90 * time.Second
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type server struct {
	mu    sync.RWMutex
	rooms map[string]*room
}

type room struct {
	id           string
	movies       []string
	currentRound int
	roundEndTime time.Time
	players      map[string]*player
	clients      map[*client]bool
	timer        *time.Timer
	isGameOver   bool
	mu           sync.Mutex
}

type player struct {
	id           string
	name         string
	score        int
	guessed      map[rune]bool
	wrongGuesses int
	status       string // "playing", "solved", "failed", "game_over"
}

type client struct {
	id   string
	name string
	conn *websocket.Conn
	send chan outboundMessage
	room *room
}

type outboundMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

type clientMessage struct {
	Type   string `json:"type"`
	Letter string `json:"letter"`
}

type roomState struct {
	RoomID       string       `json:"roomId"`
	Round        int          `json:"round"`
	TotalRounds  int          `json:"totalRounds"`
	RoundEndTime int64        `json:"roundEndTime"`
	MaskedWord   string       `json:"maskedWord"`
	WrongGuesses int          `json:"wrongGuesses"`
	MaxWrong     int          `json:"maxWrong"`
	Guessed      []string     `json:"guessed"`
	PlayerStatus string       `json:"playerStatus"`
	Answer       string       `json:"answer"`
	Players      []playerData `json:"players"`
	IsGameOver   bool         `json:"isGameOver"`
}

type playerData struct {
	Name   string `json:"name"`
	Score  int    `json:"score"`
	Status string `json:"status"`
}

func main() {
	rand.Seed(time.Now().UnixNano())

	s := &server{rooms: make(map[string]*room)}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/rooms", s.handleCreateRoom)
	mux.HandleFunc("/api/rooms/", s.handleGetRoom)
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", http.FileServer(http.Dir("./web")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	addr := ":" + port
	log.Printf("Hangman server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	roomID := randomCode(roomIDLength)
	newRoom := &room{
		id:      roomID,
		players: make(map[string]*player),
		clients: make(map[*client]bool),
		movies:  make([]string, totalRounds),
	}

	// Pre-load random movies, but do NOT start the round yet!
	perm := rand.Perm(len(moviePool))
	for i := 0; i < totalRounds; i++ {
		newRoom.movies[i] = moviePool[perm[i]]
	}

	s.mu.Lock()
	s.rooms[roomID] = newRoom
	s.mu.Unlock()

	joinURL := fmt.Sprintf("%s://%s/?room=%s", schemeFromRequest(r), r.Host, roomID)
	respondJSON(w, http.StatusCreated, map[string]string{
		"roomId":  roomID,
		"joinUrl": joinURL,
	})
}

func (s *server) handleGetRoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	roomID := strings.TrimPrefix(r.URL.Path, "/api/rooms/")
	if roomID == "" {
		http.Error(w, "room ID required", http.StatusBadRequest)
		return
	}

	rm, err := s.findRoom(roomID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	rm.mu.Lock()
	state := rm.snapshotFor(nil)
	rm.mu.Unlock()

	respondJSON(w, http.StatusOK, state)
}

func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	roomID := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("room")))
	name := sanitizeName(r.URL.Query().Get("name"))
	if roomID == "" || name == "" {
		http.Error(w, "room and name are required", http.StatusBadRequest)
		return
	}

	rm, err := s.findRoom(roomID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade failed: %v", err)
		return
	}

	cl := &client{
		id:   randomCode(8),
		name: name,
		conn: conn,
		send: make(chan outboundMessage, 32),
		room: rm,
	}

	rm.mu.Lock()
	p := &player{
		id:           cl.id,
		name:         cl.name,
		guessed:      make(map[rune]bool),
		wrongGuesses: 0,
		status:       "playing",
	}

	if rm.isGameOver {
		p.status = "game_over"
	} else if rm.currentRound == 0 {
		p.status = "waiting" // Mark as waiting if the game hasn't started
	}

	rm.players[cl.id] = p
	rm.clients[cl] = true

	playerCount := len(rm.players)
	currentRound := rm.currentRound
	rm.mu.Unlock()

	// Safely start the game when the second player joins
	if currentRound == 0 && playerCount >= 2 {
		rm.mu.Lock()
		rm.startRound(1)          // Safely starts the timer
		rm.broadcastStateLocked() // Forces all clients to see Round 1 immediately
		rm.mu.Unlock()
	} else {
		rm.broadcastState() // Otherwise, just broadcast the waiting state
	}

	go cl.writeLoop()
	cl.readLoop()
}

func (s *server) findRoom(roomID string) (*room, error) {
	s.mu.RLock()
	rm := s.rooms[roomID]
	s.mu.RUnlock()
	if rm == nil {
		return nil, errors.New("room not found")
	}
	return rm, nil
}

func (r *room) startRound(roundNum int) {
	if r.timer != nil {
		r.timer.Stop()
	}

	r.currentRound = roundNum
	if r.currentRound > totalRounds {
		r.isGameOver = true
		for _, p := range r.players {
			p.status = "game_over"
		}
		return
	}

	r.roundEndTime = time.Now().Add(roundDuration)

	for _, p := range r.players {
		p.guessed = make(map[rune]bool)
		p.wrongGuesses = 0
		p.status = "playing"
	}

	// Auto-advance when the 60s timer expires
	r.timer = time.AfterFunc(roundDuration, func() {
		r.mu.Lock()
		r.startRound(r.currentRound + 1)
		r.broadcastStateLocked()
		r.mu.Unlock()
	})
}

func (c *client) readLoop() {
	defer c.cleanup()
	c.conn.SetReadLimit(2048)
	_ = c.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		return nil
	})

	for {
		var msg clientMessage
		if err := c.conn.ReadJSON(&msg); err != nil {
			return
		}

		switch msg.Type {
		case "guess":
			if err := c.room.applyGuess(c, msg.Letter); err != nil {
				c.send <- outboundMessage{Type: "toast", Payload: map[string]string{"message": err.Error()}}
			}
		}
	}
}

func (c *client) writeLoop() {
	ticker := time.NewTicker(25 * time.Second)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteJSON(msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *client) cleanup() {
	rm := c.room
	rm.mu.Lock()
	delete(rm.clients, c)
	delete(rm.players, c.id)
	rm.mu.Unlock()
	rm.broadcastState()
	_ = c.conn.Close()
}

func (r *room) applyGuess(c *client, rawLetter string) error {
	letter := strings.ToUpper(strings.TrimSpace(rawLetter))
	if letter == "" {
		return errors.New("type a letter first")
	}

	runes := []rune(letter)
	if len(runes) != 1 || !unicode.IsLetter(runes[0]) {
		return errors.New("please guess one letter")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	p := r.players[c.id]
	if p == nil || p.status != "playing" || r.isGameOver {
		return nil
	}

	guessRune := runes[0]
	if p.guessed[guessRune] {
		return errors.New("already guessed")
	}

	p.guessed[guessRune] = true
	answer := r.movies[r.currentRound-1]

	// Determine if more than 60 seconds have passed in the 120s round
	timeRemaining := time.Until(r.roundEndTime)
	isLate := timeRemaining < 45*time.Second

	if strings.ContainsRune(strings.ToUpper(answer), guessRune) {
		// Half points if guessing took more than 60 seconds
		if isLate {
			p.score += 5
		} else {
			p.score += 10
		}

		masked := maskWord(answer, p.guessed)
		if !strings.ContainsRune(masked, '_') {
			p.status = "solved"
			// Half bonus if solving took more than 60 seconds
			if isLate {
				p.score += 25
			} else {
				p.score += 50
			}
		}
	} else {
		p.wrongGuesses++
		p.score -= 5 // Penalty for wrong guess
		if p.wrongGuesses >= maxWrongGuesses {
			p.status = "failed"
		}
	}

	// Check if all players in the room are finished (solved or failed)
	allDone := true
	activePlayers := 0
	for _, player := range r.players {
		activePlayers++
		if player.status == "playing" {
			allDone = false
			break
		}
	}

	// If everyone is done early, instantly advance to the next round
	if allDone && activePlayers > 0 {
		r.startRound(r.currentRound + 1)
	}

	r.broadcastStateLocked()
	return nil
}

func (r *room) snapshotFor(cl *client) roomState {
	var p *player
	if cl != nil {
		p = r.players[cl.id]
	}

	playersData := make([]playerData, 0, len(r.players))
	for _, player := range r.players {
		playersData = append(playersData, playerData{
			Name:   player.name,
			Score:  player.score,
			Status: player.status,
		})
	}
	sort.Slice(playersData, func(i, j int) bool {
		if playersData[i].Score == playersData[j].Score {
			return strings.ToLower(playersData[i].Name) < strings.ToLower(playersData[j].Name)
		}
		return playersData[i].Score > playersData[j].Score
	})

	state := roomState{
		RoomID:       r.id,
		Round:        r.currentRound,
		TotalRounds:  totalRounds,
		RoundEndTime: r.roundEndTime.Unix(),
		MaxWrong:     maxWrongGuesses,
		Players:      playersData,
		IsGameOver:   r.isGameOver,
	}

	if r.isGameOver || r.currentRound == 0 {
		return state
	}

	answer := r.movies[r.currentRound-1]

	if p != nil {
		state.PlayerStatus = p.status
		state.WrongGuesses = p.wrongGuesses
		state.MaskedWord = maskWord(answer, p.guessed)

		guessedLetters := make([]string, 0, len(p.guessed))
		for k := range p.guessed {
			guessedLetters = append(guessedLetters, string(k))
		}
		sort.Strings(guessedLetters)
		state.Guessed = guessedLetters

		// Reveal the answer ONLY if they have finished the round
		if p.status != "playing" {
			state.Answer = answer
		}
	}

	return state
}

func (r *room) broadcastState() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.broadcastStateLocked()
}

func (r *room) broadcastStateLocked() {
	for cl := range r.clients {
		state := r.snapshotFor(cl)
		msg := outboundMessage{Type: "state", Payload: state}

		select {
		case cl.send <- msg:
		default:
			close(cl.send)
			delete(r.clients, cl)
			delete(r.players, cl.id)
		}
	}
}

func randomCode(length int) string {
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ23456789"
	b := make([]byte, length)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	r := []rune(name)
	if len(r) > 24 {
		r = r[:24]
	}
	return string(r)
}

func maskWord(word string, guessed map[rune]bool) string {
	upperWord := strings.ToUpper(word)
	var b strings.Builder
	for _, ch := range upperWord {
		switch {
		case unicode.IsLetter(ch):
			if guessed[ch] {
				b.WriteRune(ch)
			} else {
				b.WriteRune('_')
			}
		case unicode.IsDigit(ch):
			b.WriteRune(ch)
		default:
			b.WriteRune(ch)
		}
		b.WriteRune(' ')
	}
	return strings.TrimSpace(b.String())
}

func respondJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func schemeFromRequest(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
