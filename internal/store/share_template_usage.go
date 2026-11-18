package store

import (
	"context"
	"errors"
	"regexp"
	"time"
)

var shareTemplateAssetSet = regexp.MustCompile(`^[a-f0-9]{64}$`)

type ShareTemplateUsage struct {
	ObservedSince           *time.Time `json:"observedSince"`
	RenderCount             int64      `json:"renderCount"`
	ShareCount              int64      `json:"shareCount"`
	ResourceRequestCount    int64      `json:"resourceRequestCount"`
	LastRenderedAt          *time.Time `json:"lastRenderedAt"`
	LastResourceRequestedAt *time.Time `json:"lastResourceRequestedAt"`
}

// StartShareTemplateObservation writes a zero-count resource bucket, so an
// observed zero can be distinguished from a version with unknown coverage.
// This is an observation timestamp, never a publication timestamp.
func (db *DB) StartShareTemplateObservation(ctx context.Context, assetSets []string, now time.Time) error {
	tx, err := db.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := `INSERT INTO share_template_usage_daily (asset_set,usage_day,share_id,first_observed_at)
		VALUES (?,?,'',?) ON DUPLICATE KEY UPDATE asset_set=asset_set`
	if db.database == databaseSQLite {
		query = `INSERT INTO share_template_usage_daily (asset_set,usage_day,share_id,first_observed_at)
			VALUES (?,?,'',?) ON CONFLICT(asset_set,usage_day,share_id) DO NOTHING`
	}
	for _, assetSet := range assetSets {
		if !shareTemplateAssetSet.MatchString(assetSet) {
			return errors.New("invalid template asset set")
		}
		if _, err := tx.ExecContext(ctx, query, assetSet, now.UTC().Format("2006-01-02"), databaseTime(now)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Empty shareID denotes a resource request. Only the trusted render path may
// supply a share ID; resource URLs never create share associations.
func (db *DB) RecordShareTemplateUsage(ctx context.Context, assetSet, shareID string, now time.Time) error {
	if !shareTemplateAssetSet.MatchString(assetSet) || (shareID != "" && !conversationShareID.MatchString(shareID)) {
		return errors.New("invalid template usage")
	}
	column, lastColumn := "resource_request_count", "last_resource_requested_at"
	if shareID != "" {
		column, lastColumn = "render_count", "last_rendered_at"
	}
	query := `INSERT INTO share_template_usage_daily
		(asset_set,usage_day,share_id,first_observed_at,` + column + `,` + lastColumn + `) VALUES (?,?,?,?,1,?)
		ON DUPLICATE KEY UPDATE ` + column + `=` + column + `+1, ` + lastColumn + `=GREATEST(COALESCE(` + lastColumn + `,?),?)`
	if db.database == databaseSQLite {
		query = `INSERT INTO share_template_usage_daily
			(asset_set,usage_day,share_id,first_observed_at,` + column + `,` + lastColumn + `) VALUES (?,?,?,?,1,?)
			ON CONFLICT(asset_set,usage_day,share_id) DO UPDATE SET ` + column + `=` + column + `+1,
			` + lastColumn + `=MAX(COALESCE(` + lastColumn + `,?),?)`
	}
	now = databaseTime(now)
	_, err := db.sql.ExecContext(ctx, query, assetSet, now.Format("2006-01-02"), shareID, now, now, now, now)
	return err
}

// Window counts use inclusive UTC calendar dates; latest timestamps and the
// beginning of observation cover all recorded days. No content BLOBs are read.
func (db *DB) ShareTemplateUsage(ctx context.Context, startDay, endDay string) (map[string]ShareTemplateUsage, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT asset_set, MIN(first_observed_at),
		SUM(CASE WHEN usage_day>=? AND usage_day<=? THEN render_count ELSE 0 END),
		COUNT(DISTINCT CASE WHEN usage_day>=? AND usage_day<=? AND render_count>0 THEN share_id END),
		SUM(CASE WHEN usage_day>=? AND usage_day<=? THEN resource_request_count ELSE 0 END),
		MAX(last_rendered_at), MAX(last_resource_requested_at)
		FROM share_template_usage_daily GROUP BY asset_set`, startDay, endDay, startDay, endDay, startDay, endDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]ShareTemplateUsage)
	for rows.Next() {
		var assetSet string
		var usage ShareTemplateUsage
		var observed, rendered, requested any
		if err := rows.Scan(&assetSet, &observed, &usage.RenderCount, &usage.ShareCount, &usage.ResourceRequestCount, &rendered, &requested); err != nil {
			return nil, err
		}
		for _, field := range []struct {
			raw    any
			target **time.Time
		}{{observed, &usage.ObservedSince}, {rendered, &usage.LastRenderedAt}, {requested, &usage.LastResourceRequestedAt}} {
			value, ok, err := parseSQLiteNullableTime(field.raw)
			if err != nil {
				return nil, err
			}
			if ok {
				*field.target = &value
			}
		}
		result[assetSet] = usage
	}
	return result, rows.Err()
}

type ShareTemplateShare struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	RenderCount    int64     `json:"renderCount"`
	LastRenderedAt time.Time `json:"lastRenderedAt"`
}

func (db *DB) ListShareTemplateShares(ctx context.Context, assetSet, startDay, endDay string, limit int) ([]ShareTemplateShare, error) {
	if !shareTemplateAssetSet.MatchString(assetSet) || limit < 1 || limit > 100 {
		return nil, errors.New("invalid template usage query")
	}
	rows, err := db.sql.QueryContext(ctx, `SELECT u.share_id,COALESCE(s.title,'历史对话分享'),SUM(u.render_count),MAX(u.last_rendered_at)
		FROM share_template_usage_daily u LEFT JOIN shares s ON s.id=u.share_id
		WHERE u.asset_set=? AND u.usage_day>=? AND u.usage_day<=? AND u.render_count>0
		GROUP BY u.share_id,s.title ORDER BY MAX(u.last_rendered_at) DESC,u.share_id LIMIT ?`, assetSet, startDay, endDay, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ShareTemplateShare, 0)
	for rows.Next() {
		var item ShareTemplateShare
		var last any
		if err := rows.Scan(&item.ID, &item.Title, &item.RenderCount, &last); err != nil {
			return nil, err
		}
		value, _, err := parseSQLiteNullableTime(last)
		if err != nil {
			return nil, err
		}
		item.LastRenderedAt = value
		items = append(items, item)
	}
	return items, rows.Err()
}
