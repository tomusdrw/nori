package store

import "context"

type ServiceSchedule struct {
	ID       int64
	Name     string
	CronExpr string
}

// ListServiceSchedules omits potentially large scripts and environment values
// from the scheduler's recurring configuration check.
func (s *Store) ListServiceSchedules(ctx context.Context) ([]ServiceSchedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,cron_expr FROM service WHERE policy=? AND cron_expr!=''`, PolicyScheduled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ServiceSchedule
	for rows.Next() {
		var item ServiceSchedule
		if err := rows.Scan(&item.ID, &item.Name, &item.CronExpr); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
