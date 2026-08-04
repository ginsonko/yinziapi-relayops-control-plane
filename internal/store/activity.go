package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

const activityCategorySQL = `(CASE
	WHEN kind LIKE '%mirror%' THEN 'mirror'
	WHEN kind LIKE '%price%' OR kind LIKE '%pricing%' OR kind LIKE '%proposal%' OR kind LIKE '%automation%' THEN 'price'
	WHEN kind LIKE '%balance%' OR kind LIKE '%ledger%' OR kind LIKE '%financial%' THEN 'balance'
	WHEN kind LIKE '%credential%' OR kind LIKE '%auth%' OR kind LIKE '%pair%' OR kind LIKE '%custody%' THEN 'auth'
	WHEN kind LIKE '%discovery%' OR kind LIKE '%listing%' THEN 'listing'
	WHEN kind LIKE '%config%' OR kind LIKE '%target_%' OR kind LIKE '%supplier_%' OR kind LIKE '%control%' THEN 'system'
	ELSE 'collect' END)`

type ActivityQuery struct {
	Page            int
	Limit           int
	AfterID         int64
	SkipTotal       bool
	From            *time.Time
	To              *time.Time
	Category        string
	Status          string
	ExcludeCategory string
}

type ActivityPage struct {
	Items []domain.Activity
	Page  int
	Limit int
	Total int64
}

type ActivityDeleteRequest struct {
	IDs   []int64
	Query ActivityQuery
	All   bool
	Limit int
}

func (s *Store) QueryActivities(ctx context.Context, query ActivityQuery) (ActivityPage, error) {
	query = normalizeActivityQuery(query)
	where, args, err := activityWhere(query)
	if err != nil {
		return ActivityPage{}, err
	}
	var total int64
	if !query.SkipTotal {
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM activities`+where, args...).Scan(&total); err != nil {
			return ActivityPage{}, err
		}
	}
	selectArgs := append(append([]any(nil), args...), query.Limit)
	orderAndLimit := ` ORDER BY created_at DESC,id DESC LIMIT ?`
	if query.AfterID > 0 {
		orderAndLimit = ` ORDER BY id ASC LIMIT ?`
	} else {
		selectArgs = append(selectArgs, (query.Page-1)*query.Limit)
		orderAndLimit += ` OFFSET ?`
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,severity,title,message,supplier_id,metadata_json,created_at
		FROM activities`+where+orderAndLimit, selectArgs...)
	if err != nil {
		return ActivityPage{}, err
	}
	defer rows.Close()
	items := make([]domain.Activity, 0, query.Limit)
	for rows.Next() {
		item, scanErr := scanActivity(rows)
		if scanErr != nil {
			return ActivityPage{}, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return ActivityPage{}, err
	}
	return ActivityPage{Items: items, Page: query.Page, Limit: query.Limit, Total: total}, nil
}

func (s *Store) DeleteActivities(ctx context.Context, request ActivityDeleteRequest) (int64, error) {
	limit := request.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	if len(request.IDs) > 0 {
		ids := append([]int64(nil), request.IDs...)
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		unique := ids[:0]
		for _, id := range ids {
			if id <= 0 || (len(unique) > 0 && unique[len(unique)-1] == id) {
				continue
			}
			unique = append(unique, id)
		}
		if len(unique) == 0 {
			return 0, errors.New("activity deletion requires positive IDs")
		}
		if len(unique) > limit {
			return 0, fmt.Errorf("activity deletion is limited to %d rows", limit)
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",")
		args := make([]any, len(unique))
		for index, id := range unique {
			args[index] = id
		}
		result, err := s.db.ExecContext(ctx, `DELETE FROM activities WHERE id IN (`+placeholders+`)`, args...)
		return activityRowsAffected(result, err)
	}

	if !request.All && !activityQueryHasFilter(request.Query) {
		return 0, errors.New("activity deletion requires selected IDs, an explicit filter, or all=true")
	}
	query := normalizeActivityQuery(request.Query)
	where, args, err := activityWhere(query)
	if err != nil {
		return 0, err
	}
	args = append(args, limit)
	result, err := s.db.ExecContext(ctx, `DELETE FROM activities WHERE id IN (
		SELECT id FROM activities`+where+` ORDER BY created_at DESC,id DESC LIMIT ?
	)`, args...)
	return activityRowsAffected(result, err)
}

func normalizeActivityQuery(query ActivityQuery) ActivityQuery {
	if query.Page < 1 {
		query.Page = 1
	}
	if query.Limit < 1 || query.Limit > 100 {
		query.Limit = 50
	}
	if query.AfterID > 0 && query.Limit > 50 {
		query.Limit = 50
	}
	query.Category = strings.ToLower(strings.TrimSpace(query.Category))
	query.Status = strings.ToLower(strings.TrimSpace(query.Status))
	query.ExcludeCategory = strings.ToLower(strings.TrimSpace(query.ExcludeCategory))
	return query
}

func activityWhere(query ActivityQuery) (string, []any, error) {
	conditions := make([]string, 0, 6)
	args := make([]any, 0, 6)
	if query.AfterID > 0 {
		conditions = append(conditions, "id>?")
		args = append(args, query.AfterID)
	}
	if query.From != nil {
		conditions = append(conditions, "created_at>=?")
		args = append(args, formatTime(query.From.UTC()))
	}
	if query.To != nil {
		conditions = append(conditions, "created_at<=?")
		args = append(args, formatTime(query.To.UTC()))
	}
	if query.Category != "" {
		if !validActivityCategory(query.Category) {
			return "", nil, fmt.Errorf("unsupported activity category %q", query.Category)
		}
		conditions = append(conditions, activityCategorySQL+"=?")
		args = append(args, query.Category)
	}
	if query.ExcludeCategory != "" {
		if !validActivityCategory(query.ExcludeCategory) {
			return "", nil, fmt.Errorf("unsupported excluded activity category %q", query.ExcludeCategory)
		}
		conditions = append(conditions, activityCategorySQL+"<>?")
		args = append(args, query.ExcludeCategory)
	}
	switch query.Status {
	case "":
	case "healthy":
		conditions = append(conditions, "severity IN ('success','info')")
	case "warning", "error":
		conditions = append(conditions, "severity=?")
		args = append(args, query.Status)
	case "unknown":
		conditions = append(conditions, "severity NOT IN ('success','info','warning','error')")
	default:
		return "", nil, fmt.Errorf("unsupported activity status %q", query.Status)
	}
	if len(conditions) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(conditions, " AND "), args, nil
}

func validActivityCategory(value string) bool {
	switch value {
	case "collect", "price", "balance", "auth", "listing", "mirror", "system":
		return true
	default:
		return false
	}
}

func activityQueryHasFilter(query ActivityQuery) bool {
	return query.From != nil || query.To != nil || strings.TrimSpace(query.Category) != "" ||
		strings.TrimSpace(query.Status) != "" || strings.TrimSpace(query.ExcludeCategory) != ""
}

func scanActivity(row interface{ Scan(...any) error }) (domain.Activity, error) {
	var item domain.Activity
	var supplier sql.NullInt64
	var metadata, created string
	if err := row.Scan(&item.ID, &item.Kind, &item.Severity, &item.Title, &item.Message, &supplier, &metadata, &created); err != nil {
		return domain.Activity{}, err
	}
	if supplier.Valid {
		value := supplier.Int64
		item.SupplierID = &value
	}
	_ = json.Unmarshal([]byte(metadata), &item.Metadata)
	item.CreatedAt = parseTime(created)
	return item, nil
}

func activityRowsAffected(result sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
