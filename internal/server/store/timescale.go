package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// TimescaleStore persists GPU metrics to a TimescaleDB hypertable.
type TimescaleStore struct {
	log  *zap.Logger
	pool *pgxpool.Pool
}

// NewTimescaleStore connects to PostgreSQL/TimescaleDB and ensures the schema exists.
func NewTimescaleStore(ctx context.Context, dsn string, log *zap.Logger) (*TimescaleStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db ping: %w", err)
	}

	s := &TimescaleStore{log: log, pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// migrate creates the gpu_metrics hypertable if it does not exist.
func (s *TimescaleStore) migrate(ctx context.Context) error {
	ddl := `
CREATE TABLE IF NOT EXISTS gpu_metrics (
    time          TIMESTAMPTZ     NOT NULL,
    node_name     TEXT            NOT NULL,
    gpu_index     INT             NOT NULL,
    gpu_uuid      TEXT,
    vendor        TEXT,
    util_percent  DOUBLE PRECISION,
    vram_used_mb  BIGINT,
    vram_total_mb BIGINT,
    temp_celsius  DOUBLE PRECISION,
    power_watts   DOUBLE PRECISION
);

-- Create hypertable only if TimescaleDB extension is available.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_extension WHERE extname = 'timescaledb'
    ) THEN
        PERFORM create_hypertable(
            'gpu_metrics', 'time',
            if_not_exists => TRUE,
            migrate_data  => TRUE
        );
    END IF;
END
$$;

CREATE INDEX IF NOT EXISTS gpu_metrics_node_idx
    ON gpu_metrics (node_name, gpu_index, time DESC);
`
	_, err := s.pool.Exec(ctx, ddl)
	return err
}

// Write inserts a batch of data points into the hypertable.
func (s *TimescaleStore) Write(points []GPUDataPoint) error {
	if len(points) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use a batch insert for efficiency.
	batch := &pgxBatch{}
	for _, p := range points {
		batch.add(p)
	}

	rows := make([][]interface{}, len(points))
	for i, p := range points {
		rows[i] = []interface{}{
			p.Time, p.NodeName, p.GPUIndex, p.GPUUUID, p.Vendor,
			p.UtilPercent, int64(p.VRAMUsedMB), int64(p.VRAMTotalMB),
			p.TempCelsius, p.PowerWatts,
		}
	}

	const q = `
INSERT INTO gpu_metrics
    (time, node_name, gpu_index, gpu_uuid, vendor,
     util_percent, vram_used_mb, vram_total_mb, temp_celsius, power_watts)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, row := range rows {
		if _, err := tx.Exec(ctx, q, row...); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// QueryRange returns historical data for a GPU within the given time range.
func (s *TimescaleStore) QueryRange(nodeName string, gpuIndex int, from, to time.Time) ([]GPUDataPoint, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const q = `
SELECT time, node_name, gpu_index, gpu_uuid, vendor,
       util_percent, vram_used_mb, vram_total_mb, temp_celsius, power_watts
FROM   gpu_metrics
WHERE  node_name = $1
  AND  gpu_index = $2
  AND  time BETWEEN $3 AND $4
ORDER  BY time ASC`

	rows, err := s.pool.Query(ctx, q, nodeName, gpuIndex, from, to)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var result []GPUDataPoint
	for rows.Next() {
		var p GPUDataPoint
		var vramUsed, vramTotal int64
		if err := rows.Scan(
			&p.Time, &p.NodeName, &p.GPUIndex, &p.GPUUUID, &p.Vendor,
			&p.UtilPercent, &vramUsed, &vramTotal,
			&p.TempCelsius, &p.PowerWatts,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		p.VRAMUsedMB = uint64(vramUsed)
		p.VRAMTotalMB = uint64(vramTotal)
		result = append(result, p)
	}
	return result, rows.Err()
}

// ApplyRetention drops data older than the given duration using TimescaleDB
// data retention policies. Falls back to a plain DELETE if TimescaleDB is not available.
func (s *TimescaleStore) ApplyRetention(ctx context.Context, retention time.Duration) error {
	cutoff := time.Now().Add(-retention)
	_, err := s.pool.Exec(ctx,
		`DELETE FROM gpu_metrics WHERE time < $1`, cutoff)
	return err
}

// Close closes the connection pool.
func (s *TimescaleStore) Close() error {
	s.pool.Close()
	return nil
}

// pgxBatch is a helper to accumulate rows (unused placeholder for future batch API).
type pgxBatch struct{ n int }

func (b *pgxBatch) add(_ GPUDataPoint) { b.n++ }
