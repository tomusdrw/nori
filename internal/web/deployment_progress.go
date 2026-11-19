package web

import (
	"time"

	"nori/internal/store"
)

type deploymentProgressView struct {
	ID          int64
	Status      string
	Log         string
	Elapsed     string
	Explanation string
	Poll        bool
}

func (s *Server) deploymentProgress(d *store.Deployment) deploymentProgressView {
	view := deploymentProgressView{ID: d.ID, Status: d.Status, Log: d.Log}
	var end time.Time
	switch {
	case d.Status == store.DeployPending:
		view.Poll = true
		view.Explanation = "Waiting for the deployment to start."
	case d.Status == store.DeployRunning:
		view.Poll = true
		if s.executor != nil && s.executor.DeploymentActive(d.ID) {
			end = time.Now()
			if view.Log == "" {
				view.Explanation = "Output will appear when the run finishes."
			}
		} else {
			view.Status = "unconfirmed"
			view.Explanation = "Nori can’t confirm this run is still active."
		}
	case d.FinishedAt != nil:
		end = *d.FinishedAt
	}
	if !end.IsZero() {
		view.Elapsed = max(0, end.Sub(d.StartedAt)).Truncate(time.Second).String()
	}
	return view
}
