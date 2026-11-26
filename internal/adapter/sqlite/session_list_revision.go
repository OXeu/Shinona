package sqlite

import (
	"context"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/run"
)

// SessionListRevision covers session summaries and project metadata for an account.
func (r *ConversationStore) SessionListRevision(ctx context.Context, owner string) (int64, error) {
	return sessionListRevision(r.read.WithContext(ctx), owner)
}

func sessionListRevision(db *gorm.DB, owner string) (int64, error) {
	var revision int64
	err := db.Raw(`SELECT COALESCE(
		(SELECT revision FROM session_list_revisions WHERE owner_id = ?), 0)`, owner).Row().Scan(&revision)
	return revision, err
}

func (r *ConversationStore) SessionListSnapshot(ctx context.Context, owner string, projectID *string, limit int) (run.SessionListSnapshot, error) {
	var snapshot run.SessionListSnapshot
	err := r.read.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		if snapshot.Revision, err = sessionListRevision(tx, owner); err != nil {
			return err
		}
		if snapshot.Projects, err = listProjects(tx, owner); err != nil {
			return err
		}
		snapshot.Sessions, err = listRecentSessions(tx, owner, projectID, limit)
		return err
	})
	return snapshot, err
}

func initSessionListRevisions(db *gorm.DB) error {
	for _, table := range []struct {
		name    string
		columns []string
	}{
		{"agent_sessions", []string{"owner_id", "project_id", "id", "title", "custom_title", "turn_count", "updated_at", "pinned", "read_through", "archived_at"}},
		{"projects", []string{"owner_id", "id", "name"}},
		{"runs", []string{"owner_id", "session_id", "status"}},
	} {
		var insertWhen, deleteWhen, updateWhen string
		if table.name == "runs" {
			insertWhen = "NEW.status IN ('QUEUED','RUNNING')"
			deleteWhen = "OLD.status IN ('QUEUED','RUNNING')"
			updateWhen = `(NEW.status IN ('QUEUED','RUNNING')) != (OLD.status IN ('QUEUED','RUNNING'))
				OR ((NEW.status IN ('QUEUED','RUNNING') OR OLD.status IN ('QUEUED','RUNNING'))
					AND (NEW.owner_id IS NOT OLD.owner_id OR NEW.session_id IS NOT OLD.session_id))`
		} else {
			changed := make([]string, 0, len(table.columns))
			for _, column := range table.columns {
				changed = append(changed, fmt.Sprintf("NEW.%[1]s IS NOT OLD.%[1]s", column))
			}
			updateWhen = strings.Join(changed, " OR ")
		}
		for _, operation := range []struct {
			name, event, when string
			owners            []string
		}{
			{"insert", "INSERT", insertWhen, []string{"NEW"}},
			{"delete", "DELETE", deleteWhen, []string{"OLD"}},
			{"update", "UPDATE OF " + strings.Join(table.columns, ", "), updateWhen, []string{"NEW", "OLD"}},
		} {
			when := ""
			if operation.when != "" {
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
