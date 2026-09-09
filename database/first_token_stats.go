package database

import (
	"context"
	"sort"
	"strings"
	"time"
)

type AccountFirstTokenStats struct {
	AccountID    int64   `json:"account_id"`
	AccountName  string  `json:"account_name"`
	AccountEmail string  `json:"account_email"`
	Samples      int64   `json:"samples"`
	P50Ms        float64 `json:"p50_ms"`
	P95Ms        float64 `json:"p95_ms"`
	Timeouts     int64   `json:"timeout_count"`
	Upstream500  int64   `json:"upstream_500_count"`
	Upstream502  int64   `json:"upstream_502_count"`
	Upstream503  int64   `json:"upstream_503_count"`
	Score        float64 `json:"score"`
}

func (db *DB) GetAccountFirstTokenStats(ctx context.Context, start, end time.Time) ([]AccountFirstTokenStats, error) {
	if db.driver == "sqlite3" || db.driver == "sqlite" {
		logs, err := db.ListUsageLogsByTimeRange(ctx, start, end)
		if err != nil {
			return nil, err
		}
		byID := map[int64]*AccountFirstTokenStats{}
		values := map[int64][]int{}
		for _, l := range logs {
			s := byID[l.AccountID]
			if s == nil {
				s = &AccountFirstTokenStats{AccountID: l.AccountID, AccountName: l.AccountName, AccountEmail: l.AccountEmail}
				byID[l.AccountID] = s
			}
			if l.FirstTokenMs > 0 {
				values[l.AccountID] = append(values[l.AccountID], l.FirstTokenMs)
			}
			if l.StatusCode == 500 {
				s.Upstream500++
			}
			if l.StatusCode == 502 {
				s.Upstream502++
			}
			if l.StatusCode == 503 {
				s.Upstream503++
			}
			if strings.Contains(strings.ToLower(l.ErrorMessage), "first token timeout") || strings.Contains(l.ErrorMessage, "首字超时") {
				s.Timeouts++
			}
		}
		for id, s := range byID {
			sort.Ints(values[id])
			s.Samples = int64(len(values[id]))
			if len(values[id]) > 0 {
				s.P50Ms = float64(values[id][len(values[id])/2])
				s.P95Ms = float64(values[id][int(float64(len(values[id])-1)*.95)])
			}
			s.Score = 1000/(1+s.P95Ms/1000) - float64(s.Timeouts*20+s.Upstream500*2+s.Upstream502*3+s.Upstream503*3)
		}
		result := make([]AccountFirstTokenStats, 0, len(byID))
		for _, s := range byID {
			result = append(result, *s)
		}
		sort.Slice(result, func(i, j int) bool { return result[i].P95Ms > result[j].P95Ms })
		return result, nil
	}
	q := `SELECT u.account_id, COALESCE(a.name,''), COALESCE(a.credentials->>'email',''),
 COALESCE(count(*) FILTER (WHERE u.first_token_ms > 0),0),
 COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY NULLIF(u.first_token_ms,0)),0),
 COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY NULLIF(u.first_token_ms,0)),0),
 COALESCE(count(*) FILTER (WHERE COALESCE(u.error_message,'') ILIKE '%first token timeout%' OR COALESCE(u.error_message,'') ILIKE '%首字超时%'),0),
 COALESCE(count(*) FILTER (WHERE u.status_code=500),0), COALESCE(count(*) FILTER (WHERE u.status_code=502),0), COALESCE(count(*) FILTER (WHERE u.status_code=503),0)
 FROM usage_logs u LEFT JOIN accounts a ON a.id=u.account_id WHERE u.created_at >= $1 AND u.created_at <= $2 GROUP BY u.account_id,a.name,a.credentials ORDER BY 6 DESC`
	rows, err := db.conn.QueryContext(ctx, q, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AccountFirstTokenStats{}
	for rows.Next() {
		var s AccountFirstTokenStats
		if err := rows.Scan(&s.AccountID, &s.AccountName, &s.AccountEmail, &s.Samples, &s.P50Ms, &s.P95Ms, &s.Timeouts, &s.Upstream500, &s.Upstream502, &s.Upstream503); err != nil {
			return nil, err
		}
		// Higher score means a healthier account. P95 and timeout/error rates dominate.
		s.Score = 1000/(1+s.P95Ms/1000) - float64(s.Timeouts*20+s.Upstream500*2+s.Upstream502*3+s.Upstream503*3)
		result = append(result, s)
	}
	return result, rows.Err()
}
