package collector

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
)

var (
	collectHeartbeatWrite = kingpin.Flag(
		"collect.heartbeat.write",
		"Enable background heartbeat writer.",
	).Default("false").Bool()

	collectHeartbeatWriteInterval = kingpin.Flag(
		"collect.heartbeat.write-interval",
		"Interval between heartbeat writes.",
	).Default("5s").Duration()

	collectHeartbeatWriteTimeout = kingpin.Flag(
		"collect.heartbeat.write-timeout",
		"Timeout for a single heartbeat write attempt.",
	).Default("3s").Duration()
)

const (
	heartbeatWritableQuery = `SELECT @@global.read_only, @@global.super_read_only`
	heartbeatServerIDQuery = `SELECT @@global.server_id`
)

type HeartbeatWriter struct {
	ctx      context.Context
	logger   *slog.Logger
	instance *instance
	interval time.Duration
	timeout  time.Duration
	writerID string
}

func ShouldWriteHeartbeat() bool {
	return collectHeartbeatWrite != nil && *collectHeartbeatWrite
}

func heartbeatWriterConfigured() error {
	if !ShouldWriteHeartbeat() {
		return nil
	}
	if collectHeartbeatWriteInterval == nil || *collectHeartbeatWriteInterval <= 0 {
		return fmt.Errorf("--collect.heartbeat.write-interval must be > 0")
	}
	if collectHeartbeatWriteTimeout == nil || *collectHeartbeatWriteTimeout <= 0 {
		return fmt.Errorf("--collect.heartbeat.write-timeout must be > 0")
	}
	if collectHeartbeatDatabase == nil || strings.TrimSpace(*collectHeartbeatDatabase) == "" {
		return fmt.Errorf("--collect.heartbeat.database must be set")
	}
	if collectHeartbeatTable == nil || strings.TrimSpace(*collectHeartbeatTable) == "" {
		return fmt.Errorf("--collect.heartbeat.table must be set")
	}
	return nil
}

func NewHeartbeatWriter(ctx context.Context, logger *slog.Logger, dsn string) (*HeartbeatWriter, error) {
	if err := heartbeatWriterConfigured(); err != nil {
		return nil, err
	}

	inst, err := newInstance(dsn)
	if err != nil {
		return nil, err
	}

	return &HeartbeatWriter{
		ctx:      ctx,
		logger:   logger,
		instance: inst,
		interval: *collectHeartbeatWriteInterval,
		timeout:  *collectHeartbeatWriteTimeout,
	}, nil
}

func (w *HeartbeatWriter) Start() {
	w.logger.Info("starting heartbeat writer")
	go w.loop()
}

func (w *HeartbeatWriter) loop() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	defer w.instance.Close()

	w.tickOnce()

	for {
		select {
		case <-w.ctx.Done():
			w.logger.Debug("heartbeat writer stopped")
			return
		case <-ticker.C:
			w.tickOnce()
		}
	}
}

func (w *HeartbeatWriter) tickOnce() {
	ctx, cancel := context.WithTimeout(w.ctx, w.timeout)
	defer cancel()

	if err := w.instance.Ping(); err != nil {
		w.logger.Error("heartbeat writer: ping failed", "err", err)
		return
	}

	db := w.instance.getDB()

	writable, err := isWritablePrimary(ctx, db)
	if err != nil {
		w.logger.Error("heartbeat writer: failed to check writable state", "err", err)
		return
	}
	if !writable {
		w.logger.Debug("heartbeat writer: instance is not writable, skipping write")
		return
	}

	if w.writerID == "" {
		serverID, err := getServerID(ctx, db)
		if err != nil {
			w.logger.Error("heartbeat writer: failed to fetch server_id", "err", err)
			return
		}
		w.writerID = serverID
	}

	if err := writeHeartbeat(ctx, db, w.writerID); err != nil {
		w.logger.Error("heartbeat writer: failed to write heartbeat", "err", err)
		return
	}

	w.logger.Debug("heartbeat writer: heartbeat written", "writer_id", w.writerID)
}

func isWritablePrimary(ctx context.Context, db *sql.DB) (bool, error) {
	var readOnly, superReadOnly int64

	if err := db.QueryRowContext(ctx, heartbeatWritableQuery).Scan(&readOnly, &superReadOnly); err != nil {
		return false, err
	}

	return readOnly == 0 && superReadOnly == 0, nil
}

func writeHeartbeat(ctx context.Context, db *sql.DB, writerID string) error {
	query := heartbeatWriteQuery()
	_, err := db.ExecContext(ctx, query, writerID)
	return err
}

func heartbeatWriteQuery() string {
	return fmt.Sprintf(`INSERT INTO %s.%s (server_id, ts) VALUES (?, %s) ON DUPLICATE KEY UPDATE ts = VALUES(ts)`,
		quoteIdentifier(*collectHeartbeatDatabase),
		quoteIdentifier(*collectHeartbeatTable),
		nowExpr(),
	)
}

func getServerID(ctx context.Context, db *sql.DB) (string, error) {
	var serverID int64
	if err := db.QueryRowContext(ctx, heartbeatServerIDQuery).Scan(&serverID); err != nil {
		return "", err
	}
	if serverID <= 0 {
		return "", fmt.Errorf("invalid @@global.server_id: %d", serverID)
	}
	return strconv.FormatInt(serverID, 10), nil
}
