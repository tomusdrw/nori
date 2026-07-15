package docker

import "context"

type Fake struct {
	Containers map[string][]Container
	Err        error
}

func (f *Fake) ListByService(ctx context.Context, service string) ([]Container, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Containers[service], nil
}
