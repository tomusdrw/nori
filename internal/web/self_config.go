package web

import (
	"context"
	"fmt"

	"deploybot/internal/store"
)

func (s *Server) saveSelfConfig(ctx context.Context, svc, previous *store.Service, content string) error {
	oldEnv, err := s.selfEnvironment.EditableEnvironment()
	if err != nil {
		return err
	}
	if err := s.store.RecordEnvRevision(ctx, svc.ID, oldEnv); err != nil {
		return err
	}
	if err := s.store.UpdateService(ctx, svc); err != nil {
		return err
	}
	if err := s.selfEnvironment.ReplaceEditableEnvironment(content); err != nil {
		if rollbackErr := s.store.UpdateService(ctx, previous); rollbackErr != nil {
			err = fmt.Errorf("%w; restoring prior service settings: %v", err, rollbackErr)
		}
		return err
	}
	savedEnv, err := s.selfEnvironment.EditableEnvironment()
	if err == nil {
		err = s.store.RecordEnvRevision(ctx, svc.ID, savedEnv)
	}
	if err != nil {
		if rollbackErr := s.selfEnvironment.ReplaceEditableEnvironment(oldEnv); rollbackErr != nil {
			err = fmt.Errorf("%w; restoring prior environment: %v", err, rollbackErr)
		}
		if rollbackErr := s.store.UpdateService(ctx, previous); rollbackErr != nil {
			err = fmt.Errorf("%w; restoring prior service settings: %v", err, rollbackErr)
		}
		return err
	}
	return nil
}
