// Package store 负责 SQLite 持久化：消息、投递记录与投递队列的认领。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ziwindlu/unipush/internal/model"
	_ "modernc.org/sqlite"
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("记录不存在")

// Store 封装 SQLite 连接。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库，启用 WAL 并执行迁移。
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: 数据库路径为空")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: 创建数据目录 %s: %w", dir, err)
		}
	}
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: 打开数据库: %w", err)
	}
	// SQLite 单写者模型下，单连接最稳；本服务吞吐量很小，不值得为此引入写锁竞争。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: 连接数据库 %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS messages (
		id          TEXT PRIMARY KEY,
		title       TEXT NOT NULL DEFAULT '',
		body        TEXT NOT NULL DEFAULT '',
		level       TEXT NOT NULL,
		source      TEXT NOT NULL DEFAULT '',
		targets     TEXT NOT NULL DEFAULT '[]',
		dedupe_key  TEXT NOT NULL DEFAULT '',
		link        TEXT NOT NULL DEFAULT '',
		extra       TEXT NOT NULL DEFAULT '{}',
		created_at  INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS deliveries (
		id              TEXT PRIMARY KEY,
		message_id      TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		channel         TEXT NOT NULL,
		status          TEXT NOT NULL,
		attempts        INTEGER NOT NULL DEFAULT 0,
		max_attempts    INTEGER NOT NULL,
		next_attempt_at INTEGER NOT NULL DEFAULT 0,
		last_error      TEXT NOT NULL DEFAULT '',
		last_attempt_at INTEGER,
		succeeded_at    INTEGER,
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL,
		UNIQUE (message_id, channel)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_deliveries_due ON deliveries(status, next_attempt_at)`,
	`CREATE INDEX IF NOT EXISTS idx_deliveries_message ON deliveries(message_id)`,
	// M2：路由解析结果随消息落库，投递记录保留命中的规则与渠道组。
	`ALTER TABLE messages ADD COLUMN route TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE deliveries ADD COLUMN rule TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE deliveries ADD COLUMN route_groups TEXT NOT NULL DEFAULT '[]'`,
	// M3：去重合并计数、digest 归属，以及 info 消息的聚合待办。
	`ALTER TABLE messages ADD COLUMN merged_count INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE messages ADD COLUMN aggregated_into TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE messages ADD COLUMN digest_count INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE IF NOT EXISTS aggregates (
		id          TEXT PRIMARY KEY,
		bucket      TEXT NOT NULL,
		message_id  TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		targets     TEXT NOT NULL DEFAULT '[]',
		created_at  INTEGER NOT NULL,
		digest_id   TEXT NOT NULL DEFAULT '',
		UNIQUE (message_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_aggregates_pending ON aggregates(digest_id, bucket, created_at)`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: 读取 schema 版本: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: 迁移 v%d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: 迁移 v%d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: 记录 schema 版本 v%d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: 提交迁移 v%d: %w", i+1, err)
		}
	}
	return nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error { return s.db.Close() }

// Ping 检查数据库可用性，供 /healthz 使用。
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Enqueue 在一个事务里写入消息和它的投递记录，保证「先落库再发送」。
func (s *Store) Enqueue(ctx context.Context, msg *model.Message, deliveries []*model.Delivery) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开始事务: %w", err)
	}
	defer tx.Rollback()
	if err := insertMessage(ctx, tx, msg, deliveries); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交事务: %w", err)
	}
	return nil
}

// EnqueueOptions 控制一次入队的去重与聚合行为。
type EnqueueOptions struct {
	// DedupeWindow 大于 0 且消息带 dedupe_key 时，窗口内同 key 的消息会被合并。
	DedupeWindow time.Duration
	// AggregateBucket 非空时，消息进入该聚合桶，暂不创建投递记录。
	AggregateBucket string
}

// EnqueueOutcome 是一次入队的结果。
type EnqueueOutcome struct {
	// MessageID 是本次入队的消息 ID；去重命中时是原消息的 ID。
	MessageID string
	// Merged 是去重命中后的累计合并次数。
	Merged int
	// Deduped 表示命中去重窗口，没有写入新消息与投递。
	Deduped bool
	// Aggregated 表示消息已进入聚合桶，投递等 digest 生成后统一进行。
	Aggregated bool
}

// EnqueueMessage 在同一个事务里完成去重检查、聚合入桶或常规落库，
// 保证「先落库再发送」以及并发下不会重复写入同一条 dedupe_key。
func (s *Store) EnqueueMessage(ctx context.Context, msg *model.Message, deliveries []*model.Delivery, opts EnqueueOptions) (EnqueueOutcome, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EnqueueOutcome{}, fmt.Errorf("store: 开始事务: %w", err)
	}
	defer tx.Rollback()

	if opts.DedupeWindow > 0 && msg.DedupeKey != "" {
		cutoff := msg.CreatedAt.Add(-opts.DedupeWindow).UnixMilli()
		var existingID string
		var merged int
		err := tx.QueryRowContext(ctx, `SELECT id, merged_count FROM messages
			WHERE dedupe_key = ? AND created_at >= ?
			ORDER BY created_at DESC LIMIT 1`, msg.DedupeKey, cutoff).Scan(&existingID, &merged)
		switch {
		case err == nil:
			merged++
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET merged_count = ? WHERE id = ?`, merged, existingID); err != nil {
				return EnqueueOutcome{}, fmt.Errorf("store: 累加合并次数: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return EnqueueOutcome{}, fmt.Errorf("store: 提交去重: %w", err)
			}
			return EnqueueOutcome{MessageID: existingID, Merged: merged, Deduped: true}, nil
		case !errors.Is(err, sql.ErrNoRows):
			return EnqueueOutcome{}, fmt.Errorf("store: 查询去重窗口: %w", err)
		}
	}

	if opts.AggregateBucket != "" {
		if err := insertMessage(ctx, tx, msg, nil); err != nil {
			return EnqueueOutcome{}, err
		}
		targets, err := json.Marshal(msg.Targets)
		if err != nil {
			return EnqueueOutcome{}, fmt.Errorf("store: 序列化 targets: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO aggregates (id, bucket, message_id, targets, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			model.NewID("agg_"), opts.AggregateBucket, msg.ID, string(targets), msg.CreatedAt.UnixMilli()); err != nil {
			return EnqueueOutcome{}, fmt.Errorf("store: 写入聚合桶: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return EnqueueOutcome{}, fmt.Errorf("store: 提交聚合入桶: %w", err)
		}
		return EnqueueOutcome{MessageID: msg.ID, Aggregated: true}, nil
	}

	if err := insertMessage(ctx, tx, msg, deliveries); err != nil {
		return EnqueueOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return EnqueueOutcome{}, fmt.Errorf("store: 提交事务: %w", err)
	}
	return EnqueueOutcome{MessageID: msg.ID}, nil
}

// insertMessage 在给定事务里写入消息与其投递记录。
func insertMessage(ctx context.Context, tx *sql.Tx, msg *model.Message, deliveries []*model.Delivery) error {
	targets, err := json.Marshal(msg.Targets)
	if err != nil {
		return fmt.Errorf("store: 序列化 targets: %w", err)
	}
	extra, err := json.Marshal(msg.Extra)
	if err != nil {
		return fmt.Errorf("store: 序列化 extra: %w", err)
	}
	route := ""
	if msg.Route != nil {
		encoded, err := json.Marshal(msg.Route)
		if err != nil {
			return fmt.Errorf("store: 序列化 route: %w", err)
		}
		route = string(encoded)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO messages
		(id, title, body, level, source, targets, dedupe_key, link, extra, route,
		 merged_count, aggregated_into, digest_count, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.Title, msg.Body, msg.Level, msg.Source, string(targets),
		msg.DedupeKey, msg.Link, string(extra), route,
		msg.MergedCount, msg.AggregatedInto, msg.DigestCount, msg.CreatedAt.UnixMilli()); err != nil {
		return fmt.Errorf("store: 写入消息: %w", err)
	}
	for _, d := range deliveries {
		groups, err := json.Marshal(d.Groups)
		if err != nil {
			return fmt.Errorf("store: 序列化投递渠道组: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries
			(id, message_id, channel, status, attempts, max_attempts, rule, route_groups,
			 next_attempt_at, last_error, last_attempt_at, succeeded_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.ID, d.MessageID, d.Channel, string(d.Status), d.Attempts, d.MaxAttempts,
			d.Rule, string(groups), millisOrZero(d.NextAttemptAt), d.LastError,
			millisOf(d.LastAttemptAt), millisOf(d.SucceededAt),
			d.CreatedAt.UnixMilli(), d.UpdatedAt.UnixMilli()); err != nil {
			return fmt.Errorf("store: 写入投递记录: %w", err)
		}
	}
	return nil
}

// AggregateBucket 是一个待发 digest 的聚合桶。
type AggregateBucket struct {
	Bucket string
	Count  int
	Oldest time.Time
}

// DueAggregateBuckets 返回到期的聚合桶：最早一条已经超过窗口，或条数达到 maxItems。
func (s *Store) DueAggregateBuckets(ctx context.Context, window time.Duration, maxItems int, now time.Time) ([]AggregateBucket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT bucket, COUNT(*), MIN(created_at)
		FROM aggregates WHERE digest_id = '' GROUP BY bucket ORDER BY MIN(created_at)`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询聚合桶: %w", err)
	}
	defer rows.Close()

	var due []AggregateBucket
	for rows.Next() {
		var bucket AggregateBucket
		var oldest int64
		if err := rows.Scan(&bucket.Bucket, &bucket.Count, &oldest); err != nil {
			return nil, fmt.Errorf("store: 扫描聚合桶: %w", err)
		}
		bucket.Oldest = time.UnixMilli(oldest).UTC()
		if (maxItems > 0 && bucket.Count >= maxItems) || !bucket.Oldest.Add(window).After(now) {
			due = append(due, bucket)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历聚合桶: %w", err)
	}
	return due, nil
}

// AggregateItems 返回某个桶里尚未汇总的消息，按进入桶的时间升序。
func (s *Store) AggregateItems(ctx context.Context, bucket string) ([]*model.Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+selectMessageFieldsAliased+`
		FROM messages m JOIN aggregates a ON a.message_id = m.id
		WHERE a.bucket = ? AND a.digest_id = ''
		ORDER BY a.created_at, m.id`, bucket)
	if err != nil {
		return nil, fmt.Errorf("store: 查询聚合消息: %w", err)
	}
	defer rows.Close()

	var out []*model.Message
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历聚合消息: %w", err)
	}
	return out, nil
}

// FlushAggregate 把桶里待汇总的消息标记为已汇入 digest，并写入 digest 消息与其投递记录。
// 返回被汇总的消息条数。
func (s *Store) FlushAggregate(ctx context.Context, bucket, digestID string, digest *model.Message, deliveries []*model.Delivery) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: 开始事务: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `UPDATE aggregates SET digest_id = ?
		WHERE bucket = ? AND digest_id = ''`, digestID, bucket)
	if err != nil {
		return 0, fmt.Errorf("store: 认领聚合桶: %w", err)
	}
	count, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: 统计聚合桶: %w", err)
	}
	if count == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET aggregated_into = ?
		WHERE id IN (SELECT message_id FROM aggregates WHERE digest_id = ?)`, digestID, digestID); err != nil {
		return 0, fmt.Errorf("store: 标记聚合归属: %w", err)
	}
	digest.DigestCount = int(count)
	if err := insertMessage(ctx, tx, digest, deliveries); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: 提交聚合: %w", err)
	}
	return int(count), nil
}

const selectMessageFields = `id, title, body, level, source, targets, dedupe_key,
	link, extra, route, merged_count, aggregated_into, digest_count, created_at`

const selectMessageColumns = `SELECT ` + selectMessageFields + ` FROM messages`

const selectMessageFieldsAliased = `m.id, m.title, m.body, m.level, m.source, m.targets, m.dedupe_key,
	m.link, m.extra, m.route, m.merged_count, m.aggregated_into, m.digest_count, m.created_at`

// GetMessage 读取一条消息，不存在时返回 ErrNotFound。
func (s *Store) GetMessage(ctx context.Context, id string) (*model.Message, error) {
	row := s.db.QueryRowContext(ctx, selectMessageColumns+` WHERE id = ?`, id)
	msg, err := scanMessage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return msg, nil
}

func scanMessage(row scanner) (*model.Message, error) {
	msg := &model.Message{}
	var targets, extra, route string
	var createdAt int64
	if err := row.Scan(&msg.ID, &msg.Title, &msg.Body, &msg.Level, &msg.Source, &targets,
		&msg.DedupeKey, &msg.Link, &extra, &route, &msg.MergedCount, &msg.AggregatedInto,
		&msg.DigestCount, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: 读取消息: %w", err)
	}
	if err := json.Unmarshal([]byte(targets), &msg.Targets); err != nil {
		return nil, fmt.Errorf("store: 解析 targets: %w", err)
	}
	if err := json.Unmarshal([]byte(extra), &msg.Extra); err != nil {
		return nil, fmt.Errorf("store: 解析 extra: %w", err)
	}
	if route != "" {
		msg.Route = &model.Route{}
		if err := json.Unmarshal([]byte(route), msg.Route); err != nil {
			return nil, fmt.Errorf("store: 解析 route: %w", err)
		}
	}
	msg.CreatedAt = time.UnixMilli(createdAt).UTC()
	return msg, nil
}

// ListAggregatedMessages 返回被汇总进某条 digest 的原始消息 ID（按进入桶的顺序）。
func (s *Store) ListAggregatedMessages(ctx context.Context, digestID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT message_id FROM aggregates
		WHERE digest_id = ? ORDER BY created_at, message_id`, digestID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 digest 组成: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: 扫描 digest 组成: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 digest 组成: %w", err)
	}
	return ids, nil
}

// ListDeliveries 读取一条消息的全部投递记录。
func (s *Store) ListDeliveries(ctx context.Context, messageID string) ([]*model.Delivery, error) {
	rows, err := s.db.QueryContext(ctx, selectDeliveryColumns+
		` WHERE message_id = ? ORDER BY created_at, channel`, messageID)
	if err != nil {
		return nil, fmt.Errorf("store: 读取投递记录: %w", err)
	}
	defer rows.Close()

	var out []*model.Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历投递记录: %w", err)
	}
	return out, nil
}

const selectDeliveryColumns = `SELECT id, message_id, channel, status, attempts, max_attempts,
	rule, route_groups, next_attempt_at, last_error, last_attempt_at, succeeded_at, created_at, updated_at
	FROM deliveries`

// ClaimDue 认领最多 limit 条到期投递，并把它们标记为 sending。
// 认领会把 attempts 加一，同时把 next_attempt_at 推到租约到期时间；
// 进程崩溃后，重启时的 RequeueInFlight 或租约到期都会让它们重新被认领。
func (s *Store) ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]*model.Delivery, error) {
	if limit <= 0 {
		limit = 1
	}
	if lease <= 0 {
		lease = time.Minute
	}
	now := time.Now().UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: 认领投递: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, selectDeliveryColumns+`
		WHERE status IN (?, ?) AND next_attempt_at <= ?
		ORDER BY next_attempt_at, created_at, channel
		LIMIT ?`,
		string(model.DeliveryPending), string(model.DeliverySending), now.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询到期投递: %w", err)
	}
	var claimed []*model.Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		claimed = append(claimed, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: 遍历到期投递: %w", err)
	}
	rows.Close()

	leaseUntil := now.Add(lease)
	for _, d := range claimed {
		d.Status = model.DeliverySending
		d.Attempts++
		d.LastAttemptAt = &now
		d.NextAttemptAt = &leaseUntil
		d.UpdatedAt = now
		if _, err := tx.ExecContext(ctx, `UPDATE deliveries
			SET status = ?, attempts = ?, last_attempt_at = ?, next_attempt_at = ?, updated_at = ?
			WHERE id = ?`,
			string(d.Status), d.Attempts, now.UnixMilli(), leaseUntil.UnixMilli(), now.UnixMilli(), d.ID); err != nil {
			return nil, fmt.Errorf("store: 认领投递 %s: %w", d.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: 提交认领: %w", err)
	}
	return claimed, nil
}

// RequeueInFlight 把所有处于 sending 的投递重新放回队列，用于进程重启后的恢复。
func (s *Store) RequeueInFlight(ctx context.Context) (int64, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE deliveries
		SET status = ?, next_attempt_at = ?, updated_at = ?
		WHERE status = ?`,
		string(model.DeliveryPending), now.UnixMilli(), now.UnixMilli(), string(model.DeliverySending))
	if err != nil {
		return 0, fmt.Errorf("store: 恢复在途投递: %w", err)
	}
	return res.RowsAffected()
}

// MarkSucceeded 把一条投递标记为成功。
func (s *Store) MarkSucceeded(ctx context.Context, id string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE deliveries
		SET status = ?, succeeded_at = ?, last_error = '', next_attempt_at = 0, updated_at = ?
		WHERE id = ?`,
		string(model.DeliverySucceeded), now.UnixMilli(), now.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("store: 标记投递成功: %w", err)
	}
	return nil
}

// Reschedule 把一条失败的投递放回队列，等待下一次重试。
func (s *Store) Reschedule(ctx context.Context, id string, next time.Time, lastErr string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE deliveries
		SET status = ?, next_attempt_at = ?, last_error = ?, updated_at = ?
		WHERE id = ?`,
		string(model.DeliveryPending), next.UTC().UnixMilli(), lastErr, now.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("store: 重新排队投递: %w", err)
	}
	return nil
}

// MarkFailed 把一条投递标记为最终失败。
func (s *Store) MarkFailed(ctx context.Context, id string, lastErr string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE deliveries
		SET status = ?, last_error = ?, next_attempt_at = 0, updated_at = ?
		WHERE id = ?`,
		string(model.DeliveryFailed), lastErr, now.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("store: 标记投递失败: %w", err)
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanDelivery(row scanner) (*model.Delivery, error) {
	d := &model.Delivery{}
	var (
		status                              string
		groups                              string
		nextAttempt, lastAttempt, succeeded sql.NullInt64
		createdAt, updatedAt                int64
	)
	if err := row.Scan(&d.ID, &d.MessageID, &d.Channel, &status, &d.Attempts, &d.MaxAttempts,
		&d.Rule, &groups, &nextAttempt, &d.LastError, &lastAttempt, &succeeded,
		&createdAt, &updatedAt); err != nil {
		return nil, fmt.Errorf("store: 扫描投递记录: %w", err)
	}
	if groups != "" {
		if err := json.Unmarshal([]byte(groups), &d.Groups); err != nil {
			return nil, fmt.Errorf("store: 解析投递渠道组: %w", err)
		}
	}
	d.Status = model.DeliveryStatus(status)
	d.NextAttemptAt = timePtr(nextAttempt)
	d.LastAttemptAt = timePtr(lastAttempt)
	d.SucceededAt = timePtr(succeeded)
	d.CreatedAt = time.UnixMilli(createdAt).UTC()
	d.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return d, nil
}

func timePtr(n sql.NullInt64) *time.Time {
	if !n.Valid || n.Int64 == 0 {
		return nil
	}
	t := time.UnixMilli(n.Int64).UTC()
	return &t
}

func millisOrZero(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UTC().UnixMilli()
}

func millisOf(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().UnixMilli()
}
