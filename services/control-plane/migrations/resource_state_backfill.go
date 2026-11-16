package migrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

const resourceStateBackfillBatchSize = 500

// Each statement advances the checkpoint with the capture of the selected resources. Lock live and
// deleted organizations before their resource updates so an organization transition serializes with
// the capture. Rows inserted behind the checkpoint were already captured by the insert trigger.
const backfillResourceStateBatch = `
WITH checkpoint AS MATERIALIZED (
    SELECT after_id, last_id FROM resource_state_backfill WHERE singleton AND completed_at IS NULL FOR UPDATE
), candidates AS MATERIALIZED (
    SELECT r.id, r.organization_id FROM resources r CROSS JOIN checkpoint c
    WHERE r.id > c.after_id AND r.id <= c.last_id
    ORDER BY r.id LIMIT $1
), locked_orgs AS MATERIALIZED (
    SELECT o.id FROM organizations o WHERE o.id IN (SELECT organization_id FROM candidates)
    ORDER BY o.id FOR SHARE
), enrolled AS (
    UPDATE resources r SET state_enrolled = true
    FROM candidates c JOIN locked_orgs o ON o.id = c.organization_id
    WHERE r.id = c.id AND NOT r.state_enrolled
    RETURNING r.id
), batch_end AS (
    SELECT id FROM candidates ORDER BY id DESC LIMIT 1
)
UPDATE resource_state_backfill SET
    after_id = coalesce((SELECT id FROM batch_end), after_id),
    completed_at = CASE WHEN NOT EXISTS (SELECT 1 FROM batch_end) THEN clock_timestamp() END
WHERE singleton AND EXISTS (SELECT 1 FROM checkpoint)
RETURNING (SELECT count(*) FROM enrolled), completed_at IS NOT NULL
`

func backfillResourceState(ctx context.Context, db *sql.DB, logger *slog.Logger) error {
	for {
		var rows int64
		var completed bool
		err := db.QueryRowContext(ctx, backfillResourceStateBatch, resourceStateBackfillBatchSize).Scan(&rows, &completed)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("migrations: backfill resource state: %w", err)
		case completed:
			logger.InfoContext(ctx, "resource state backfill complete")
			return nil
		default:
			logger.InfoContext(ctx, "resource state backfill batch committed", slog.Int64("resources_enrolled", rows))
		}
	}
}
