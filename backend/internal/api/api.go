package api

import (
	"net/http"

	"github.com/JP-Go/semana-go-react/backend/internal/store/pgstore"
	"github.com/go-chi/chi/v5"
)

type ApiHandler struct {
	q *pgstore.Queries
	r *chi.Mux
}

func (h ApiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	h.r.ServeHTTP(w, r)
}

func NewApiHandler(q *pgstore.Queries) http.Handler {
	h := ApiHandler{
		q: q,
	}
	r := chi.NewMux()
	h.r = r
	return h
}
