package web

import (
	"context"
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"

	"deploybot/internal/docker"
	"deploybot/internal/store"
)

type LatestDigestFunc func(ctx context.Context, image string) (string, error)

type Server struct {
	store  *store.Store
	docker docker.Client
	latest LatestDigestFunc
	router chi.Router
}

func NewServer(st *store.Store, dk docker.Client, latest LatestDigestFunc) *Server {
	s := &Server{store: st, docker: dk, latest: latest}
	r := chi.NewRouter()
	r.Get("/", s.handleDashboard)
	r.Get("/partials/services", s.handleServicesPartial)
	sub, _ := fs.Sub(staticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	s.router = r
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.router.ServeHTTP(w, r) }

func (s *Server) loadViews(ctx context.Context) ([]ServiceView, error) {
	svcs, err := s.store.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]ServiceView, 0, len(svcs))
	for _, svc := range svcs {
		cs, err := s.docker.ListByService(ctx, svc.Name)
		if err != nil {
			return nil, err
		}
		v := ServiceView{
			Name:           svc.Name,
			State:          summarizeState(cs),
			RunningVersion: shortDigest(watchedDigest(cs, svc.WatchedImage)),
		}
		if latest, err := s.latest(ctx, svc.WatchedImage); err == nil {
			v.LatestVersion = shortDigest(latest)
			running := watchedDigest(cs, svc.WatchedImage)
			v.UpdateAvailable = running != "" && running != latest
		} else {
			v.LatestVersion = "—"
		}
		views = append(views, v)
	}
	return views, nil
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	views, err := s.loadViews(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = Page(views).Render(r.Context(), w)
}

func (s *Server) handleServicesPartial(w http.ResponseWriter, r *http.Request) {
	views, err := s.loadViews(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = ServicesTable(views).Render(r.Context(), w)
}
