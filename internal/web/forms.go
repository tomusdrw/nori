package web

import "strconv"

func strconvItoa(i int) string {
	return strconv.Itoa(i)
}

type ServiceFormData struct {
	Name         string
	WatchedImage string
	Policy       string
	CronExpr     string
	DeployScript string
	EnvFile      string
	IsSelf       bool
}

type ServiceDetailData struct {
	Service     ServiceFormData
	State       string
	Running     string
	Latest      string
	UpdateAvail bool
	Containers  []ContainerView
	Deployments []DeploymentView
}

type ContainerView struct {
	Name  string
	Image string
	State string
}

type DeploymentView struct {
	ID           int64
	Trigger      string
	TargetDigest string
	Status       string
	StartedAt    string
}
