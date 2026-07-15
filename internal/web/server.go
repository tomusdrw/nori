package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"deploybot/internal/auth"
	"deploybot/internal/docker"
	"deploybot/internal/envfile"
	"deploybot/internal/executor"
	"deploybot/internal/poller"
	"deploybot/internal/store"
	terminalsession "deploybot/internal/terminal"
)

type Server struct {
	store    *store.Store
	docker   docker.Client
	executor *executor.Executor
	poller   *poller.Poller
	auth     *auth.Auth
	terminal terminalsession.Attacher
	router   chi.Router
}

func NewServer(st *store.Store, dk docker.Client, ex *executor.Executor, pl *poller.Poller, a *auth.Auth, terminals ...terminalsession.Attacher) *Server {
	term := terminalsession.Attacher(terminalsession.New("deploybot", "."))
	if len(terminals) > 0 && terminals[0] != nil {
		term = terminals[0]
	}
	s := &Server{store: st, docker: dk, executor: ex, poller: pl, auth: a, terminal: term}
	r := chi.NewRouter()
	r.Get("/login", s.handleLoginGet)
	r.Post("/login", s.handleLoginPost)
	r.Post("/logout", s.handleLogout)

	r.Group(func(r chi.Router) {
		r.Use(s.auth.Middleware)
		r.Use(s.auth.CSRFMiddleware)

		r.Get("/", s.handleDashboard)
		r.Get("/terminal", s.handleTerminal)
		r.Get("/terminal/ws", s.handleTerminalWebSocket)
		r.Get("/partials/services", s.handleServicesPartial)
		r.Get("/services/new", s.handleServiceNew)
		r.Post("/validate/editor", s.handleEditorValidate)
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

func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	unavailable := ""
	if err := s.terminal.Available(); err != nil {
		unavailable = err.Error()
	}
	_ = TerminalPage(s.csrf(r), unavailable).Render(r.Context(), w)
}

type terminalClientMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

var terminalUpgrader = websocket.Upgrader{
	HandshakeTimeout: 5 * time.Second,
	CheckOrigin:      terminalOriginAllowed,
}

func terminalOriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) handleTerminalWebSocket(w http.ResponseWriter, r *http.Request) {
	if err := s.terminal.Available(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	ws, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(1 << 20)

	client, err := s.terminal.Attach(r.Context(), 24, 80)
	if err != nil {
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error()), time.Now().Add(time.Second))
		return
	}
	defer client.Close()

	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := client.Read(buffer)
			if n > 0 {
				if writeErr := ws.WriteMessage(websocket.BinaryMessage, buffer[:n]); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				_ = ws.Close()
				return
			}
		}
	}()

	for {
		_, payload, err := ws.ReadMessage()
		if err != nil {
			break
		}
		var message terminalClientMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			continue
		}
		switch message.Type {
		case "input":
			if message.Data != "" {
				if _, err := client.Write([]byte(message.Data)); err != nil {
					return
				}
			}
		case "resize":
			rows, cols := terminalDimensions(message.Rows, message.Cols)
			_ = client.Resize(rows, cols)
		case "ping":
			// Application-level keepalive. Receiving the frame is sufficient.
		}
	}

	_ = client.Close()
	select {
	case <-outputDone:
	case <-time.After(time.Second):
	}
}

func terminalDimensions(rows, cols uint16) (uint16, uint16) {
	rows = min(max(rows, 2), 500)
	cols = min(max(cols, 2), 500)
	return rows, cols
}

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
			Managed:        svc.IsSelf,
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
	if err := validateServiceForm(r.Context(), form); err != nil {
		_ = ServiceFormPage(form, s.csrf(r), false, "/services", err.Error()).Render(r.Context(), w)
		return
	}
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
	if err := s.store.SetEnvFile(r.Context(), svc.ID, form.EnvFile); err != nil {
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
	if svc.IsSelf {
		// The launcher owns the self image and handoff command. Policy remains
		// editable, but a POST cannot turn the managed service into an arbitrary
		// privileged Docker script.
		form.Name = svc.Name
		form.WatchedImage = svc.WatchedImage
		form.DeployScript = store.SelfDeployScript
		form.EnvFile = ""
		form.IsSelf = true
	}
	if err := validateServiceForm(r.Context(), form); err != nil {
		_ = ServiceFormPage(form, s.csrf(r), true, "/services/"+svc.Name, err.Error()).Render(r.Context(), w)
		return
	}
	svc.WatchedImage = form.WatchedImage
	svc.Policy = store.Policy(form.Policy)
	svc.CronExpr = form.CronExpr
	svc.DeployScript = form.DeployScript
	if err := s.store.UpdateService(r.Context(), svc); err != nil {
		_ = ServiceFormPage(form, s.csrf(r), true, "/services/"+svc.Name, err.Error()).Render(r.Context(), w)
		return
	}
	if !svc.IsSelf {
		if err := s.store.SetEnvFile(r.Context(), svc.ID, form.EnvFile); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/services/"+svc.Name, http.StatusSeeOther)
}

func (s *Server) handleServiceDelete(w http.ResponseWriter, r *http.Request) {
	svc, err := s.getServiceByName(w, r)
	if err != nil {
		return
	}
	if svc.IsSelf {
		http.Error(w, "the managed self-service cannot be deleted", http.StatusForbidden)
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
	fmt.Fprint(w, html.EscapeString(d.Log))
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
	fmt.Fprint(w, html.EscapeString(b.String()))
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
		Containers:  cviews, Deployments: dviews,
	}, nil
}

func (s *Server) serviceToForm(ctx context.Context, svc *store.Service) (ServiceFormData, error) {
	content, err := s.store.GetEnvFile(ctx, svc.ID)
	if err != nil {
		return ServiceFormData{}, err
	}
	return ServiceFormData{
		Name: svc.Name, WatchedImage: svc.WatchedImage, Policy: string(svc.Policy),
		CronExpr: svc.CronExpr, DeployScript: svc.DeployScript, EnvFile: content, IsSelf: svc.IsSelf,
	}, nil
}

func parseServiceForm(r *http.Request) ServiceFormData {
	// Browsers submit <textarea> content with CRLF newlines; normalize to
	// LF so the stored script/env matches what was validated and what Bash
	// can parse.
	return ServiceFormData{
		Name: r.FormValue("name"), WatchedImage: r.FormValue("watched_image"),
		Policy: r.FormValue("policy"), CronExpr: r.FormValue("cron_expr"),
		DeployScript: executor.NormalizeNewlines(r.FormValue("deploy_script")),
		EnvFile:      executor.NormalizeNewlines(r.FormValue("env_file")),
	}
}

func validateServiceForm(ctx context.Context, form ServiceFormData) error {
	if _, err := envfile.Parse(form.EnvFile); err != nil {
		return fmt.Errorf("environment file: %w", err)
	}
	if err := executor.ValidateScript(ctx, form.DeployScript); err != nil {
		return fmt.Errorf("deploy script: %w", err)
	}
	return nil
}

type editorValidation struct {
	Valid   bool   `json:"valid"`
	Message string `json:"message,omitempty"`
	Line    int    `json:"line,omitempty"`
}

var validationLinePattern = regexp.MustCompile(`(?i)line[ :]+([0-9]+)`)

func (s *Server) handleEditorValidate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var err error
	switch r.FormValue("kind") {
	case "bash":
		err = executor.ValidateScript(r.Context(), r.FormValue("content"))
	case "dotenv":
		_, err = envfile.Parse(r.FormValue("content"))
	default:
		http.Error(w, "unknown editor kind", http.StatusBadRequest)
		return
	}

	result := editorValidation{Valid: err == nil}
	if err != nil {
		result.Message = err.Error()
		if match := validationLinePattern.FindStringSubmatch(result.Message); len(match) == 2 {
			result.Line, _ = strconv.Atoi(match[1])
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
