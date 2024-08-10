package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/JP-Go/semana-go-react/backend/internal/store/pgstore"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
)

const (
	MessageKindMessageCreated   = "message_created"
	MessageKindReactedToMessage = "reacted_to_message"
	MessageKindRemovedReaction  = "removed_reaction"
	MessageKindAnswered         = "message_answered"
	ReactionKindRemoved         = "remove"
	ReactionKindAdded           = "add"
)

type ApiHandler struct {
	q           *pgstore.Queries
	r           *chi.Mux
	upgrader    *websocket.Upgrader
	subscribers map[string]map[*websocket.Conn]context.CancelFunc
	mu          *sync.Mutex
}

type Message struct {
	Kind   string `json:"kind"`
	Value  any    `json:"value"`
	RoomID string `json:"-"`
}
type MessageCreatedMessage struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

type MessageAnswered struct {
	ID string `json:"id"`
}

type ReactedMessage struct {
	MessageID     string `json:"message_id"`
	ReactionCount int64  `json:"reaction_count"`
	ReactionKind  string `json:"reaction_kind"`
}

func (h ApiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}

func NewApiHandler(q *pgstore.Queries) http.Handler {
	h := ApiHandler{
		q: q,
		upgrader: &websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		subscribers: make(map[string]map[*websocket.Conn]context.CancelFunc),
		mu:          &sync.Mutex{},
	}
	r := chi.NewMux()
	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		AllowCredentials: false,
		ExposedHeaders:   []string{"Link"},
		MaxAge:           300,
	}))

	r.Get("/subscribe/{room_id}", h.handleSubscribe)

	r.Route("/api", func(r chi.Router) {
		r.Route("/rooms", func(r chi.Router) {
			r.Post("/", h.handleCreateRoom)
			r.Get("/", h.handleGetRooms)

			r.Route("/{room_id}/messages", func(r chi.Router) {
				r.Get("/", h.handleGetRoomMessages)
				r.Post("/", h.handleCreateMessage)

				r.Route("/{message_id}", func(r chi.Router) {
					r.Get("/", h.handleGetRoomMessage)
					r.Patch("/react", h.handleReactToMessage)
					r.Delete("/react", h.handleRemoveReaction)
					r.Patch("/answer", h.handleMarkMessageAsRead)
				})
			})
		})

	})
	h.r = r
	return h
}

func (h ApiHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {

	rawRoomID := chi.URLParam(r, "room_id")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusNotFound)
			return
		}
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
	}

	c, err := h.upgrader.Upgrade(w, r, nil)

	if err != nil {
		slog.Warn("Failed to upgrade connection", err)
		http.Error(w, "Failed to upgrade connection.", http.StatusBadRequest)
		return
	}

	defer c.Close()
	h.mu.Lock()
	ctx, cancel := context.WithCancel(r.Context())
	if _, ok := h.subscribers[rawRoomID]; !ok {
		h.subscribers[rawRoomID] = make(map[*websocket.Conn]context.CancelFunc)
	}

	slog.Info("connected", "client ip", r.RemoteAddr, "room_id", rawRoomID)
	h.subscribers[rawRoomID][c] = cancel
	h.mu.Unlock()

	<-ctx.Done()

	h.mu.Lock()
	delete(h.subscribers[rawRoomID], c)
	h.mu.Unlock()
}

func (h ApiHandler) notifyClient(m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	subsscribers, ok := h.subscribers[m.RoomID]
	if !ok || len(subsscribers) == 0 {
		return
	}

	for conn, cancel := range subsscribers {
		if err := conn.WriteJSON(m); err != nil {
			slog.Warn("Failed to send message to client", "error: ", err)
			cancel()
		}
	}
}

func (h ApiHandler) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	type _body struct {
		Theme string `json:"theme"`
	}
	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	roomId, err := h.q.InsertRoom(r.Context(), body.Theme)
	if err != nil {
		slog.Error("Failed to create room", err)
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}
	type response struct {
		RoomId string `json:"room_id"`
	}
	data, _ := json.Marshal(response{RoomId: roomId.String()})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)

}

func (h ApiHandler) handleCreateMessage(w http.ResponseWriter, r *http.Request) {

	rawRoomID := chi.URLParam(r, "room_id")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusNotFound)
			return
		}
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}
	type _body struct {
		Message string `json:"message"`
	}
	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	messageID, err := h.q.InsertMessage(r.Context(), pgstore.InsertMessageParams{
		RoomID:  roomID,
		Message: body.Message,
	})

	if err != nil {
		slog.Error("Failed to create message", "error:", err)
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}

	type response struct {
		MessageId string `json:"id"`
	}

	go h.notifyClient(Message{
		RoomID: rawRoomID,
		Kind:   MessageKindMessageCreated,
		Value: MessageCreatedMessage{
			ID:      messageID.String(),
			Message: body.Message,
		},
	})

	data, _ := json.Marshal(response{MessageId: messageID.String()})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)

}

func (h ApiHandler) handleGetRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := h.q.GetRooms(r.Context())
	if err != nil {
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
	}

	type Room struct {
		ID    string `json:"id"`
		Theme string `json:"theme"`
	}

	type response struct {
		Rooms []Room `json:"rooms"`
	}

	responseRooms := []Room{}
	for _, room := range rooms {
		responseRooms = append(responseRooms, Room{ID: room.ID.String(), Theme: room.Theme})
	}
	w.Header().Set("Content-type", "application/json")
	data, _ := json.Marshal(response{Rooms: responseRooms})
	_, _ = w.Write(data)

}
func (h ApiHandler) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {

	rawRoomID := chi.URLParam(r, "room_id")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusNotFound)
			return
		}
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}

	messages, err := h.q.GetRoomMessages(r.Context(), roomID)
	if err != nil {
		slog.Error("Failed to get messages", "error: ", err)
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}

	type responseMessage struct {
		ID            string `json:"id"`
		Message       string `json:"message"`
		Answered      bool   `json:"answered"`
		ReactionCount int64  `json:"reaction_count"`
	}

	type response struct {
		RoomID   string            `json:"room_id"`
		Messages []responseMessage `json:"messages"`
	}
	responseMessages := []responseMessage{}
	for _, message := range messages {
		responseMessages = append(responseMessages, responseMessage{
			ID:            message.ID.String(),
			Message:       message.Message,
			Answered:      message.Answered,
			ReactionCount: message.ReactionCount,
		})
	}

	w.Header().Set("Content-type", "application/json")
	data, _ := json.Marshal(response{RoomID: rawRoomID, Messages: responseMessages})
	_, _ = w.Write(data)
}
func (h ApiHandler) handleReactToMessage(w http.ResponseWriter, r *http.Request) {
	rawRoomID := chi.URLParam(r, "room_id")
	rawMessageID := chi.URLParam(r, "message_id")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}
	messageID, err := uuid.Parse(rawMessageID)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusNotFound)
			return
		}
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}
	_, err = h.q.GetMessage(r.Context(), messageID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusNotFound)
			return
		}
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}

	count, err := h.q.ReactToMessage(r.Context(), messageID)

	if err != nil {
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-type", "application/json")
	data, _ := json.Marshal(struct {
		ID            string `json:"id"`
		ReactionCount int64  `json:"reaction_count"`
	}{
		ID:            messageID.String(),
		ReactionCount: count,
	})

	go h.notifyClient(Message{
		Kind:   MessageKindReactedToMessage,
		Value:  ReactedMessage{MessageID: messageID.String(), ReactionCount: count, ReactionKind: ReactionKindAdded},
		RoomID: rawRoomID,
	})
	_, _ = w.Write(data)

}
func (h ApiHandler) handleRemoveReaction(w http.ResponseWriter, r *http.Request) {

	rawRoomID := chi.URLParam(r, "room_id")
	rawMessageID := chi.URLParam(r, "message_id")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}
	messageID, err := uuid.Parse(rawMessageID)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusNotFound)
			return
		}
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}
	_, err = h.q.GetMessage(r.Context(), messageID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusNotFound)
			return
		}
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}

	count, err := h.q.RemoveReactionFromMessage(r.Context(), messageID)

	if err != nil {
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-type", "application/json")
	data, _ := json.Marshal(struct {
		ID            string `json:"id"`
		ReactionCount int64  `json:"reaction_count"`
	}{
		ID:            messageID.String(),
		ReactionCount: count,
	})

	go h.notifyClient(Message{
		Kind:   MessageKindReactedToMessage,
		Value:  ReactedMessage{MessageID: messageID.String(), ReactionCount: count, ReactionKind: ReactionKindRemoved},
		RoomID: rawRoomID,
	})
	_, _ = w.Write(data)

}
func (h ApiHandler) handleMarkMessageAsRead(w http.ResponseWriter, r *http.Request) {
	rawRoomID := chi.URLParam(r, "room_id")
	rawMessageID := chi.URLParam(r, "message_id")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}
	messageID, err := uuid.Parse(rawMessageID)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusNotFound)
			return
		}
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}
	_, err = h.q.GetMessage(r.Context(), messageID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusNotFound)
			return
		}
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}

	err = h.q.MarkMessageAsAnswered(r.Context(), messageID)

	if err != nil {
		http.Error(w, "something went wrong. try again later", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-type", "application/json")
	w.WriteHeader(http.StatusNoContent)
	go h.notifyClient(Message{
		Kind:   MessageKindAnswered,
		Value:  MessageAnswered{ID: messageID.String()},
		RoomID: rawRoomID,
	})

}
func (h ApiHandler) handleGetRoomMessage(w http.ResponseWriter, r *http.Request) {}
