package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"nori/internal/store"
)

type dashboardView struct {
	Groups []dashboardGroup
	Count  int
}

type dashboardGroup struct {
	store.ServiceGroup
	Services []ServiceView
}

func (s *Server) loadDashboard(ctx context.Context) (dashboardView, error) {
	views, err := s.loadViews(ctx)
	if err != nil {
		return dashboardView{}, err
	}
	groups, err := s.store.ListServiceGroups(ctx)
	if err != nil {
		return dashboardView{}, err
	}
	dashboard := dashboardView{Count: len(views)}
	groupIndex := make(map[string]int, len(groups))
	for i, group := range groups {
		groupIndex[group.Name] = i
		dashboard.Groups = append(dashboard.Groups, dashboardGroup{ServiceGroup: group})
	}
	for _, view := range views {
		index, ok := groupIndex[view.Group]
		if !ok {
			// Membership can change between the two reads. Keep the card
			// visible until the next refresh can show the new placement.
			index = groupIndex[store.DefaultServiceGroup]
		}
		dashboard.Groups[index].Services = append(dashboard.Groups[index].Services, view)
	}
	return dashboard, nil
}

type groupStatus struct {
	State string
	Label string
	Count int
}

func (g dashboardGroup) statuses() []groupStatus {
	counts := make(map[string]int)
	for _, svc := range g.Services {
		counts[svc.State]++
	}
	var statuses []groupStatus
	for _, status := range []groupStatus{
		{State: "running", Label: "running"},
		{State: "partial", Label: "partial"},
		{State: "stopped", Label: "stopped"},
		{State: "none", Label: "without containers"},
		{State: "unknown", Label: "unknown"},
	} {
		if status.Count = counts[status.State]; status.Count > 0 {
			statuses = append(statuses, status)
		}
	}
	return statuses
}

func (s *Server) handleServiceGroupMove(w http.ResponseWriter, r *http.Request) {
	id, ok := serviceGroupID(w, r)
	if !ok {
		return
	}
	target, err := strconv.ParseInt(r.PostFormValue("target"), 10, 64)
	before, beforeErr := strconv.ParseBool(r.PostFormValue("before"))
	if err != nil || target <= 0 || beforeErr != nil {
		http.Error(w, "Invalid group position", http.StatusBadRequest)
		return
	}
	if err := s.store.MoveServiceGroup(r.Context(), id, target, before); err != nil {
		serviceGroupError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		s.handleServicesPartial(w, r)
		return
	}
	redirectToGroup(w, r, id)
}

func (s *Server) handleServiceGroupFold(w http.ResponseWriter, r *http.Request) {
	id, ok := serviceGroupID(w, r)
	if !ok {
		return
	}
	collapsed, err := strconv.ParseBool(r.PostFormValue("collapsed"))
	if err != nil {
		http.Error(w, "Invalid folded state", http.StatusBadRequest)
		return
	}
	if err := s.store.SetServiceGroupCollapsed(r.Context(), id, collapsed); err != nil {
		serviceGroupError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		s.handleServicesPartial(w, r)
		return
	}
	redirectToGroup(w, r, id)
}

func serviceGroupID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

func serviceGroupError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	log.Printf("service groups: %s %s: %v", r.Method, r.URL.Path, err)
	http.Error(w, "Could not access service groups", http.StatusInternalServerError)
}

func serviceGroupURL(id int64) string {
	return "/service-groups/" + strconv.FormatInt(id, 10)
}

func serviceGroupAnchor(id int64) string {
	return "service-group-" + strconv.FormatInt(id, 10)
}

func redirectToGroup(w http.ResponseWriter, r *http.Request, id int64) {
	http.Redirect(w, r, "/#"+serviceGroupAnchor(id), http.StatusSeeOther)
}
