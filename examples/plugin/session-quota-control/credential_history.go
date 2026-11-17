package main

import "time"

type credentialHistory struct {
	Executions    int64     `json:"executions"`
	Failed        int64     `json:"failed"`
	TotalTokens   int64     `json:"total_tokens"`
	LastRequestAt time.Time `json:"last_request_at"`
}

// credentialHistory shares the plugin's usage database and caches its aggregate
// for five seconds so dashboard polling does not repeatedly scan request history.
func (s *reportStore) credentialHistory(now time.Time) (map[string]*credentialHistory, bool, string) {
	s.mu.Lock()
	if !s.enabled || s.db == nil {
		s.mu.Unlock()
		return nil, false, ""
	}
	if s.history != nil && now.Sub(s.historyAt) < 5*time.Second {
		history := s.history
		s.mu.Unlock()
		return history, true, ""
	}
	db := s.db
	s.mu.Unlock()
	// Aggregation must not hold the mutex used by lifecycle and usage writes.
	rows, errQuery := db.Query(`
		SELECT COALESCE(NULLIF(json_extract(e.value, '$.auth_index'), ''), json_extract(e.value, '$.auth_id')),
		       COUNT(*), SUM(CASE WHEN json_extract(e.value, '$.failed') THEN 1 ELSE 0 END),
		       SUM(MAX(0, COALESCE(NULLIF(json_extract(e.value, '$.tokens.TotalTokens'), 0),
		           COALESCE(json_extract(e.value, '$.tokens.InputTokens'), 0) + COALESCE(json_extract(e.value, '$.tokens.OutputTokens'), 0)))),
		       MAX(COALESCE(unixepoch(json_extract(e.value, '$.timestamp')), u.ts / 1000))
		FROM usage_events AS u, json_each(CASE WHEN json_valid(u.payload) THEN u.payload ELSE '{}' END, '$.executions') AS e
		WHERE COALESCE(NULLIF(json_extract(e.value, '$.auth_index'), ''), json_extract(e.value, '$.auth_id'), '') <> ''
		GROUP BY 1`)
	if errQuery != nil {
		return nil, false, "cannot read credential usage history"
	}
	history := make(map[string]*credentialHistory)
	for rows.Next() {
		var index string
		var latest int64
		entry := &credentialHistory{}
		if errScan := rows.Scan(&index, &entry.Executions, &entry.Failed, &entry.TotalTokens, &latest); errScan != nil {
			_ = rows.Close()
			return nil, false, "cannot decode credential usage history"
		}
		entry.LastRequestAt = time.Unix(latest, 0)
		history[index] = entry
	}
	errRows, errClose := rows.Err(), rows.Close()
	if errRows != nil || errClose != nil {
		return nil, false, "cannot finish reading credential usage history"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != db || !s.enabled {
		return nil, false, ""
	}
	s.history, s.historyAt = history, now
	return history, true, ""
}
