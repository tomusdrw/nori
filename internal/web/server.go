package web

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"deploybot/internal/auth"
	"deploybot/internal/docker"
	"deploybot/internal/executor"
	"deploybot/internal/poller"
	"deploybot/internal/store"
)

type Server struct {
	store    *store.Store
	docker   docker.Client
	executor *executor.Executor
	poller   *poller.Poller
	auth     *auth.Auth
	router   chi.Router
}

func NewServer(st *store.Store, dk docker.Client, ex *executor.Executor, pl *poller.Poller, a *auth.Auth) *Server {
	s := &Server{store: st, docker: dk, executor: ex, poller: pl, auth: a}
	r := chi.NewRouter()
	r.Get("/login", s.handleLoginGet)
	r.Post("/login", s.handleLoginPost)
	r.Post("/logout", s.handleLogout)

	r.Group(func(r chi.Router) {
		r.Use(s.auth.Middleware)
		r.Use(s.auth.CSRFMiddleware)

		r.Get("/", s.handleDashboard)
		r.Get("/partials/services", s.handleServicesPartial)
		r.Get("/services/new", s.handleServiceNew)
		r.Post("/services", s.handleServiceCreate)
		r.Get("/services/{name}", s.handleServiceDetail)
		r.Get("/services/{name}/edit", s.handleServiceEdit)
		r.Post("/services/{name}", s.handleServiceUpdate)
		r.Post("/services/{name}/delete", s.handleServiceDelete)
		r.Post("/services/{name}/deploy", s.handleDeploy)
		r.Post("/services/{name}/start", s.handleStart)
		r.Post("/services/{name}/stop", s.handleStop)
		r.Get("/services/{name}/logs/stream", s.handleLogsStream)
		r.Get("/deployments/{id}", s.handleDeployment)
		r.Get("/deployments/{id}/stream", s.handleDeploymentStream)
	})

	sub, _ := fs.Sub(staticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	s.router = r
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.router.ServeHTTP(w, r) }

func (s *Server) csrf(r *http.Request) string { return s.auth.CSRFToken(r) }

func (s *Server) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	_ = LoginPage("", "").Render(r.Context(), w)
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.auth.Login(w, r, r.FormValue("password")); err != nil {
		_ = LoginPage("", "Invalid credentials").Render(r.Context(), w)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) loadViews(ctx context.Context) ([]ServiceView, error) {
	svcs, err := s.store.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]ServiceView, 0, len(svcs))
	for _, svc := range svcs {
		cs, err := s.docker.ListByService(ctx, svc.Name)
		state := "unknown"
		runningDigest := ""
		if err == nil {
			state = summarizeState(cs)
			runningDigest = watchedDigest(cs, svc.WatchedImage)
		}
		latest := s.poller.CachedDigest(svc.Name)
		v := ServiceView{
			ID:             svc.ID,
			Name:           svc.Name,
			State:          state,
			RunningVersion: shortDigest(runningDigest),
			LatestVersion:  shortDigest(latest),
		}
		v.UpdateAvailable = runningDigest != "" && latest != "" && runningDigest != latest
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
	_ = Page(views, s.csrf(r)).Render(r.Context(), w)
}

func (s *Server) handleServicesPartial(w http.ResponseWriter, r *http.Request) {
	views, err := s.loadViews(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = ServicesTablePartial(views, s.csrf(r)).Render(r.Context(), w)
}

func (s *Server) handleServiceNew(w http.ResponseWriter, r *http.Request) {
	_ = ServiceFormPage(ServiceFormData{Policy: "manual"}, s.csrf(r), false, "/services", "").Render(r.Context(), w)
}

func (s *Server) handleServiceCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	form := parseServiceForm(r)
	svc := &store.Service{
		Name:         form.Name,
		WatchedImage: form.WatchedImage,
		Policy:       store.Policy(form.Policy),
		CronExpr:     form.CronExpr,
		DeployScript: form.DeployScript,
	}
	if err := s.store.CreateService(r.Context(), svc); err != nil {
		_ = ServiceFormPage(form, s.csrf(r), false, "/services", err.Error()).Render(r.Context(), w)
		return
	}
	if err := s.saveEnvVars(r.Context(), svc.ID, form.EnvVars); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/services/"+svc.Name, http.StatusSeeOther)
}

func (s *Server) handleServiceEdit(w http.ResponseWriter, r *http.Request) {
	svc, err := s.getServiceByName(w, r)
	if err != nil {
		return
	}
	form, err := s.serviceToForm(r.Context(), svc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = ServiceFormPage(form, s.csrf(r), true, "/services/"+svc.Name, "").Render(r.Context(), w)
}

func (s *Server) handleServiceUpdate(w http.ResponseWriter, r *http.Request) {
	svc, err := s.getServiceByName(w, r)
	if err != nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	form := parseServiceForm(r)
	svc.WatchedImage = form.WatchedImage
	svc.Policy = store.Policy(form.Policy)
	svc.CronExpr = form.CronExpr
	svc.DeployScript = form.DeployScript
	if err := s.store.UpdateService(r.Context(), svc); err != nil {
		_ = ServiceFormPage(form, s.csrf(r), true, "/services/"+svc.Name, err.Error()).Render(r.Context(), w)
		return
	}
	if err := s.saveEnvVars(r.Context(), svc.ID, form.EnvVars); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/services/"+svc.Name, http.StatusSeeOther)
}

func (s *Server) handleServiceDelete(w http.ResponseWriter, r *http.Request) {
	svc, err := s.getServiceByName(w, r)
	if err != nil {
		return
	}
	if err := s.store.DeleteService(r.Context(), svc.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	svc, err := s.getServiceByName(w, r)
	if err != nil {
		return
	}
	id, err := s.executor.Deploy(r.Context(), svc.ID, store.TriggerManual)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/deployments/%d", id), http.StatusSeeOther)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.docker.StartByService(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/services/"+name, http.StatusSeeOther)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.docker.StopByService(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/services/"+name, http.StatusSeeOther)
}

func (s *Server) handleServiceDetail(w http.ResponseWriter, r *http.Request) {
	svc, err := s.getServiceByName(w, r)
	if err != nil {
		return
	}
	data, err := s.buildDetail(r.Context(), svc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = ServiceDetailPage(data, s.csrf(r)).Render(r.Context(), w)
}

func (s *Server) handleDeployment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	d, err := s.store.GetDeployment(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	_ = DeploymentPage(d.ID, d.Status, d.Log, s.csrf(r)).Render(r.Context(), w)
}

func (s *Server) handleDeploymentStream(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	d, err := s.store.GetDeployment(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, d.Log)
	if d.Status == store.DeployRunning || d.Status == store.DeployPending {
		w.Header().Set("HX-Trigger", "continue")
	}
}

func (s *Server) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	cs, err := s.docker.ListByService(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var b strings.Builder
	for _, c := range cs {
		rc, err := s.docker.Logs(r.Context(), c.ID, 100)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "=== %s ===\n", c.Name)
		scanner := bufio.NewScanner(rc)
		for scanner.Scan() {
			line := scanner.Text()
			if len(line) > 8 {
				line = line[8:]
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		rc.Close()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, b.String())
}

func (s *Server) getServiceByName(w http.ResponseWriter, r *http.Request) (*store.Service, error) {
	svc, err := s.store.GetServiceByName(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, err
	}
	return svc, nil
}

func (s *Server) buildDetail(ctx context.Context, svc *store.Service) (ServiceDetailData, error) {
	form, err := s.serviceToForm(ctx, svc)
	if err != nil {
		return ServiceDetailData{}, err
	}
	cs, err := s.docker.ListByService(ctx, svc.Name)
	state := "unknown"
	running := ""
	if err == nil {
		state = summarizeState(cs)
		running = shortDigest(watchedDigest(cs, svc.WatchedImage))
	}
	latest := s.poller.CachedDigest(svc.Name)
	deploys, err := s.store.ListDeployments(ctx, svc.ID, 20)
	if err != nil {
		return ServiceDetailData{}, err
	}
	var dviews []DeploymentView
	for _, d := range deploys {
		dviews = append(dviews, DeploymentView{
			ID: d.ID, Trigger: d.Trigger, TargetDigest: d.TargetDigest,
			Status: d.Status, StartedAt: d.StartedAt.Format(time.RFC3339),
		})
	}
	var cviews []ContainerView
	for _, c := range cs {
		cviews = append(cviews, ContainerView{Name: c.Name, Image: c.Image, State: c.State})
	}
	return ServiceDetailData{
		Service: form, State: state, Running: running, Latest: shortDigest(latest),
		UpdateAvail: running != "" && latest != "" && running != latest,
		Containers: cviews, Deployments: dviews,
	}, nil
}

func (s *Server) serviceToForm(ctx context.Context, svc *store.Service) (ServiceFormData, error) {
	vars, err := s.store.ListEnvVars(ctx, svc.ID)
	if err != nil {
		return ServiceFormData{}, err
	}
	var evs []EnvVarFormData
	for _, v := range vars {
		evs = append(evs, EnvVarFormData{Key: v.Key, Value: v.Value, IsSecret: v.IsSecret})
	}
	return ServiceFormData{
		Name: svc.Name, WatchedImage: svc.WatchedImage, Policy: string(svc.Policy),
		CronExpr: svc.CronExpr, DeployScript: svc.DeployScript, EnvVars: evs,
	}, nil
}

func (s *Server) saveEnvVars(ctx context.Context, serviceID int64, vars []EnvVarFormData) error {
	for _, ev := range vars {
		if ev.Key == "" {
			continue
		}
		if err := s.store.SetEnvVar(ctx, &store.EnvVar{
			ServiceID: serviceID, Key: ev.Key, Value: ev.Value, IsSecret: ev.IsSecret,
		}); err != nil {
			return err
		}
	}
	return nil
}

func parseServiceForm(r *http.Request) ServiceFormData {
	form := ServiceFormData{
		Name: r.FormValue("name"), WatchedImage: r.FormValue("watched_image"),
		Policy: r.FormValue("policy"), CronExpr: r.FormValue("cron_expr"),
		DeployScript: r.FormValue("deploy_script"),
	}
	for i := 0; i < 50; i++ {
		key := r.FormValue("env_key_" + strconv.Itoa(i))
		if key == "" {
			continue
		}
		form.EnvVars = append(form.EnvVars, EnvVarFormData{
			Key: key, Value: r.FormValue("env_val_" + strconv.Itoa(i)),
			IsSecret: r.FormValue("env_secret_"+strconv.Itoa(i)) == "on",
		})
	}
	return form
}
