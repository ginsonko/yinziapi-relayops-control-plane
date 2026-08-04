package store

import (
	"context"
	"fmt"
	"time"
)

func (s *Store) compactHistoricalNoChangeProposals(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,policy_id FROM change_proposals
		WHERE status='no_change' ORDER BY policy_id,id DESC`)
	if err != nil {
		return fmt.Errorf("list historical no-change proposals: %w", err)
	}
	latestByPolicy := make(map[int64]int64)
	var allIDs, obsoleteIDs []int64
	for rows.Next() {
		var id, policyID int64
		if err := rows.Scan(&id, &policyID); err != nil {
			rows.Close()
			return err
		}
		allIDs = append(allIDs, id)
		if _, exists := latestByPolicy[policyID]; exists {
			obsoleteIDs = append(obsoleteIDs, id)
		} else {
			latestByPolicy[policyID] = id
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	removedActivities := int64(0)
	for _, proposalID := range allIDs {
		metadataPattern := `%"proposal_id":"` + fmt.Sprint(proposalID) + `"%`
		result, err := tx.ExecContext(ctx, `DELETE FROM activities WHERE kind='proposal_created' AND metadata_json LIKE ?`, metadataPattern)
		if err != nil {
			return fmt.Errorf("compact historical no-change activity: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		removedActivities += count
	}
	for _, proposalID := range obsoleteIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM change_proposals WHERE id=? AND status='no_change'`, proposalID); err != nil {
			return fmt.Errorf("compact historical no-change proposal: %w", err)
		}
	}
	if len(obsoleteIDs) > 0 || removedActivities > 0 {
		metadata := mustJSON(map[string]string{
			"removed_proposals":  fmt.Sprint(len(obsoleteIDs)),
			"removed_activities": fmt.Sprint(removedActivities),
		})
		if _, err := tx.ExecContext(ctx, `INSERT INTO activities(kind,severity,title,message,metadata_json,created_at)
			VALUES('maintenance','info','已整理历史无变化记录',?,?,?)`,
			fmt.Sprintf("保留每条规则最新状态，移除 %d 条重复记录和 %d 条误导活动", len(obsoleteIDs), removedActivities),
			metadata, formatTime(time.Now().UTC())); err != nil {
			return fmt.Errorf("record no-change compaction: %w", err)
		}
	}
	return tx.Commit()
}
