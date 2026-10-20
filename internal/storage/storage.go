package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Store struct{ DB *sql.DB }
type Server struct {
	ID, Name, Address string
	Priority          int
	AgentURL          string
}
type Sample struct {
	ServerID, Status, Internet, AgentStatus, Error string
	CPU, RAMUsed, RAMTotal, DiskUsed, DiskTotal    float64
	Uptime                                         uint64
	CheckedAt                                      time.Time
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	schema := `CREATE TABLE IF NOT EXISTS servers(id TEXT PRIMARY KEY,name TEXT NOT NULL,address TEXT NOT NULL,priority INTEGER NOT NULL,agent_url TEXT NOT NULL); CREATE TABLE IF NOT EXISTS samples(id INTEGER PRIMARY KEY,server_id TEXT NOT NULL,checked_at DATETIME NOT NULL,status TEXT NOT NULL,internet TEXT NOT NULL,agent_status TEXT NOT NULL,cpu REAL NOT NULL,ram_used REAL NOT NULL,ram_total REAL NOT NULL,disk_used REAL NOT NULL,disk_total REAL NOT NULL,error TEXT NOT NULL,uptime INTEGER NOT NULL DEFAULT 0); CREATE INDEX IF NOT EXISTS samples_server_time ON samples(server_id,checked_at DESC); CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY,occurred_at DATETIME NOT NULL,server_id TEXT NOT NULL,kind TEXT NOT NULL,message TEXT NOT NULL); CREATE INDEX IF NOT EXISTS events_time ON events(occurred_at DESC); CREATE TABLE IF NOT EXISTS users(user_id INTEGER PRIMARY KEY,username TEXT NOT NULL DEFAULT '',role TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1); CREATE TABLE IF NOT EXISTS invitations(username TEXT PRIMARY KEY,invited_by INTEGER NOT NULL,created_at DATETIME NOT NULL); CREATE TABLE IF NOT EXISTS service_state(key TEXT PRIMARY KEY,value TEXT NOT NULL,updated_at DATETIME NOT NULL);`
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`ALTER TABLE samples ADD COLUMN uptime INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}
func (s *Store) SyncServers(ctx context.Context, servers []Server) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, v := range servers {
		if _, err = tx.ExecContext(ctx, `INSERT INTO servers(id,name,address,priority,agent_url) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,address=excluded.address,priority=excluded.priority,agent_url=excluded.agent_url`, v.ID, v.Name, v.Address, v.Priority, v.AgentURL); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Save(ctx context.Context, m Sample) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO samples(server_id,checked_at,status,internet,agent_status,cpu,ram_used,ram_total,disk_used,disk_total,error,uptime) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, m.ServerID, m.CheckedAt.UTC(), m.Status, m.Internet, m.AgentStatus, m.CPU, m.RAMUsed, m.RAMTotal, m.DiskUsed, m.DiskTotal, m.Error, m.Uptime)
	return err
}
func (s *Store) Samples(ctx context.Context, serverID string, since time.Time, limit int) ([]Sample, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT server_id,status,internet,agent_status,cpu,ram_used,ram_total,disk_used,disk_total,checked_at,error,uptime FROM samples WHERE server_id=? AND checked_at>=? ORDER BY checked_at`, serverID, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Sample
	for rows.Next() {
		var m Sample
		if err := rows.Scan(&m.ServerID, &m.Status, &m.Internet, &m.AgentStatus, &m.CPU, &m.RAMUsed, &m.RAMTotal, &m.DiskUsed, &m.DiskTotal, &m.CheckedAt, &m.Error, &m.Uptime); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if limit > 0 && len(result) > limit {
		stride := (len(result) + limit - 1) / limit
		sampled := make([]Sample, 0, limit)
		for i := 0; i < len(result); i += stride {
			sampled = append(sampled, result[i])
		}
		result = sampled
	}
	return result, nil
}
func (s *Store) Event(ctx context.Context, server, kind, msg string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO events(occurred_at,server_id,kind,message) VALUES(?,?,?,?)`, time.Now().UTC(), server, kind, msg)
	return err
}
func (s *Store) Prune(ctx context.Context, days int) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM samples WHERE checked_at < ?`, time.Now().AddDate(0, 0, -days))
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `DELETE FROM events WHERE occurred_at < ?`, time.Now().AddDate(0, 0, -days))
	return err
}
func (s *Store) RunBackups(ctx context.Context, dir string, interval time.Duration, retentionDays int) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if retentionDays <= 0 {
		retentionDays = 14
	}
	if err := s.backup(ctx, dir, retentionDays); err != nil {
		slog.Error("ошибка резервного копирования SQLite", "error", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.backup(ctx, dir, retentionDays); err != nil {
				slog.Error("ошибка резервного копирования SQLite", "error", err)
			}
		}
	}
}
func (s *Store) backup(ctx context.Context, dir string, retentionDays int) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	name := "uptime-" + time.Now().UTC().Format("20060102T150405.000000000") + ".db"
	path, err := filepath.Abs(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	if _, err = s.DB.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "uptime-") || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Store) ProvisionAdmins(ctx context.Context, ids []int64) error {
	for _, id := range ids {
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO users(user_id,role,enabled) VALUES(?,'admin',1) ON CONFLICT(user_id) DO UPDATE SET role='admin',enabled=1`, id); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) Allowed(ctx context.Context, id int64) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE user_id=? AND enabled=1`, id).Scan(&n)
	return n > 0, err
}
func normalizeUsername(v string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(v), "@"))
}
func (s *Store) Invite(ctx context.Context, username string, by int64) error {
	u := normalizeUsername(username)
	if len(u) < 5 || len(u) > 32 {
		return fmt.Errorf("некорректный username")
	}
	for _, r := range u {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return fmt.Errorf("некорректный username")
		}
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO invitations(username,invited_by,created_at) VALUES(?,?,?) ON CONFLICT(username) DO UPDATE SET invited_by=excluded.invited_by,created_at=excluded.created_at`, u, by, time.Now().UTC())
	return err
}
func (s *Store) AcceptInvite(ctx context.Context, id int64, username string) (bool, error) {
	u := normalizeUsername(username)
	if u == "" {
		return false, nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var invited bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM invitations WHERE username=?)`, u).Scan(&invited)
	if err != nil || !invited {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO users(user_id,username,role,enabled) VALUES(?,?,'user',1) ON CONFLICT(user_id) DO UPDATE SET username=excluded.username,enabled=1`, id, u); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM invitations WHERE username=?`, u); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
func (s *Store) Users(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT user_id,username,role,enabled FROM users ORDER BY role,user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		var u, role string
		var enabled int
		if err = rows.Scan(&id, &u, &role, &enabled); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%d @%s %s %s", id, u, role, map[bool]string{true: "активен", false: "отключён"}[enabled == 1]))
	}
	return out, rows.Err()
}
func (s *Store) DisableUser(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET enabled=0 WHERE user_id=? AND role!='admin'`, id)
	return err
}
func (s *Store) UserIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT user_id FROM users WHERE enabled=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s *Store) Close() error { return s.DB.Close() }
