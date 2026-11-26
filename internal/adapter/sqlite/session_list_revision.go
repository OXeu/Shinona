package sqlite

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// SessionListRevision covers the list projection for an account, including
// project metadata shared by all of its filters.
func (r *ConversationStore) SessionListRevision(ctx context.Context, owner string) (int64, error) {
	var revision int64
	err := r.read.WithContext(ctx).Raw(`SELECT COALESCE(
		(SELECT revision FROM session_list_revisions WHERE owner_id = ?), 0)`, owner).Row().Scan(&revision)
	return revision, err
}

func initSessionListRevisions(db *gorm.DB) error {
	for _, table := range []struct {
		name, columns string
	}{
		{"agent_sessions", "owner_id, project_id, id, title, custom_title, turn_count, updated_at, pinned, read_through, archived_at"},
		{"projects", "owner_id, id, name"},
		{"runs", "owner_id, session_id, status"},
	} {
		for _, operation := range []struct {
			name, event, when string
			owners            []string
		}{
			{"insert", "INSERT", "NEW.status IN ('QUEUED','RUNNING')", []string{"NEW"}},
			{"delete", "DELETE", "OLD.status IN ('QUEUED','RUNNING')", []string{"OLD"}},
			{"update", "UPDATE OF " + table.columns,
				`(NEW.status IN ('QUEUED','RUNNING')) != (OLD.status IN ('QUEUED','RUNNING'))
					OR ((NEW.status IN ('QUEUED','RUNNING') OR OLD.status IN ('QUEUED','RUNNING'))
					AND (NEW.owner_id IS NOT OLD.owner_id OR NEW.session_id IS NOT OLD.session_id))`, []string{"NEW", "OLD"}},
		} {
			when := ""
			if table.name == "runs" {
				when = "WHEN " + operation.when
			}
			var changes string
			for _, owner := range operation.owners {
				// A child deleted by the account cascade must not recreate
				// a revision for an account that has just been removed.
				predicate := fmt.Sprintf("EXISTS (SELECT 1 FROM users WHERE id = %[1]s.owner_id)", owner)
				if operation.name == "update" && owner == "OLD" {
					predicate += " AND NEW.owner_id IS NOT OLD.owner_id"
				}
				changes += fmt.Sprintf(`INSERT INTO session_list_revisions (owner_id, revision)
						SELECT %[1]s.owner_id, 1 WHERE %[2]s
						ON CONFLICT(owner_id) DO UPDATE SET revision = revision + 1; `, owner, predicate)
			}
			name := fmt.Sprintf("session_list_%s_%s", table.name, operation.name)
			if err := db.Exec("DROP TRIGGER IF EXISTS " + name).Error; err != nil {
				return err
			}
			trigger := fmt.Sprintf(`CREATE TRIGGER %s AFTER %s ON %s %s BEGIN %s END`, name, operation.event, table.name, when, changes)
			if err := db.Exec(trigger).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
