package sqlite

import (
	"fmt"

	"gorm.io/gorm"
)

// The v6 layouts retain the empty project ID used by the account's default
// project. A nullable generated key enforces ownership of named projects.
const (
	accountOwner = "owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE"
	projectRef   = "project_ref TEXT GENERATED ALWAYS AS (NULLIF(project_id, '')) VIRTUAL"
	projectFK    = "FOREIGN KEY (owner_id, project_ref) REFERENCES projects (owner_id, id)"
	sessionFK    = "FOREIGN KEY (owner_id, session_id) REFERENCES agent_sessions (owner_id, id) ON DELETE CASCADE"
)

var ownershipLayouts = []struct {
	name, columns, definition string
}{
	{"projects", "id, owner_id, name",
		"id TEXT PRIMARY KEY, " + accountOwner + ", name TEXT NOT NULL, UNIQUE (owner_id, id)"},
	{"agent_sessions", "id, owner_id, project_id, title, turn_count, created_at, updated_at, pinned, custom_title, read_through, archived_at",
		"id TEXT PRIMARY KEY, " + accountOwner + `, project_id TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL, turn_count INTEGER NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
		pinned NUMERIC NOT NULL DEFAULT false, custom_title NUMERIC NOT NULL DEFAULT false,
		read_through INTEGER NOT NULL DEFAULT 0, archived_at DATETIME,
		` + projectRef + ", UNIQUE (owner_id, id), " + projectFK},
	{"runs", `id, owner_id, session_id, retry_of, sequence, policy, backend, prompt, payload, steering, parts,
		agent_messages, compacted_context, context_tokens, context_window, token_usage, status,
		output, created_at, started_at, updated_at, reverted_at`,
		`id TEXT PRIMARY KEY, owner_id TEXT, session_id TEXT, retry_of TEXT, sequence INTEGER, policy TEXT,
		backend TEXT, prompt TEXT NOT NULL, payload TEXT, steering TEXT, parts TEXT,
		agent_messages BLOB, compacted_context BLOB, context_tokens INTEGER, context_window INTEGER,
		token_usage TEXT, status TEXT NOT NULL, output TEXT, created_at DATETIME NOT NULL,
		started_at DATETIME, updated_at DATETIME NOT NULL, reverted_at DATETIME,
		CHECK ((owner_id IS NULL) = (session_id IS NULL)), ` + sessionFK},
	{"workspace_roots", "owner_id, session_id, root",
		"owner_id TEXT NOT NULL, session_id TEXT NOT NULL, root TEXT NOT NULL, PRIMARY KEY (owner_id, session_id), " + sessionFK},
	{"project_sandbox_settings", "owner_id, project_id, provider, template, timeout_seconds, on_timeout",
		accountOwner + `, project_id TEXT NOT NULL, provider TEXT, template TEXT, timeout_seconds INTEGER, on_timeout TEXT,
		` + projectRef + ", PRIMARY KEY (owner_id, project_id), " + projectFK + " ON DELETE CASCADE"},
	{"project_workspace_roots", "owner_id, project_id, root",
		accountOwner + ", project_id TEXT NOT NULL, root TEXT NOT NULL, " + projectRef +
			", PRIMARY KEY (owner_id, project_id), " + projectFK + " ON DELETE CASCADE"},
	{"e2b_project_workspaces", "owner_id, project_id, endpoint, sandbox_id, credentials",
		accountOwner + `, project_id TEXT NOT NULL, endpoint TEXT NOT NULL, sandbox_id TEXT NOT NULL, credentials BLOB NOT NULL,
		` + projectRef + ", PRIMARY KEY (owner_id, project_id, endpoint), " + projectFK + " ON DELETE CASCADE"},
	{"session_list_revisions", "owner_id, revision",
		accountOwner + ", revision INTEGER NOT NULL, PRIMARY KEY (owner_id)"},
	{"sandbox_settings", "owner_id, policy", accountOwner + ", policy TEXT NOT NULL, PRIMARY KEY (owner_id)"},
	{"composer_defaults", "owner_id, model, permission, thinking",
		accountOwner + ", model TEXT NOT NULL, permission TEXT NOT NULL, thinking TEXT NOT NULL, PRIMARY KEY (owner_id)"},
	{"model_settings", "owner_id, data", accountOwner + ", data BLOB NOT NULL, PRIMARY KEY (owner_id)"},
	{"mcp_settings", "owner_id, data", accountOwner + ", data BLOB NOT NULL, PRIMARY KEY (owner_id)"},
	{"mcp_oauth_grants", "owner_id, server, grant",
		accountOwner + ", server TEXT, grant BLOB NOT NULL, PRIMARY KEY (owner_id, server)"},
	{"ssh_servers", "owner_id, id, name, address, user, host_fingerprint, template, private_key",
		accountOwner + `, id TEXT, name TEXT NOT NULL, address TEXT, user TEXT, host_fingerprint TEXT,
		template TEXT, private_key TEXT, PRIMARY KEY (owner_id, id)`},
	{"account_avatars", "owner_id, version, media_type, data",
		accountOwner + ", version TEXT, media_type TEXT, data BLOB, PRIMARY KEY (owner_id)"},
	{"account_email_challenges", "id, owner_id, session_key, email, code_hash, sent_at, expires_at, attempts",
		"id TEXT, " + accountOwner + `, session_key TEXT, email TEXT, code_hash TEXT,
		sent_at DATETIME, expires_at DATETIME, attempts INTEGER, PRIMARY KEY (owner_id)`},
	{"oauth_identities", "issuer, subject, owner_id", "issuer TEXT, subject TEXT, " + accountOwner + ", PRIMARY KEY (issuer, subject)"},
	{"login_sessions", "key, owner_id, expires_at, authenticated_at",
		"key TEXT PRIMARY KEY, " + accountOwner + ", expires_at DATETIME NOT NULL, authenticated_at DATETIME"},
	{"oauth_flows", "key, nonce, verifier, expires_at, version, owner_id, session_key",
		`key TEXT PRIMARY KEY, nonce TEXT NOT NULL, verifier TEXT NOT NULL, expires_at DATETIME NOT NULL, version TEXT,
		owner_id TEXT REFERENCES users(id) ON DELETE CASCADE, session_key TEXT`},
}

func migrateOwnership(db *gorm.DB) error {
	tables := make([]string, len(ownershipLayouts))
	for i, layout := range ownershipLayouts {
		tables[i] = layout.name
	}
	var definitions []string
	if err := db.Raw(`SELECT sql FROM sqlite_schema
		WHERE tbl_name IN ? AND sql IS NOT NULL AND (
			type = 'index' OR (type = 'trigger' AND name NOT GLOB 'reject_deleted_owner_*'
				AND name NOT GLOB 'session_list_*')
		) ORDER BY type, name`, tables).Scan(&definitions).Error; err != nil {
		return err
	}
	for _, layout := range ownershipLayouts {
		name, source, suffix := layout.name, layout.columns, ""
		from := name
		switch name {
		case "runs":
			// The adoption command can still assign pre-login runs to an account.
			source = `id, NULLIF(owner_id, ''), CASE WHEN NULLIF(owner_id, '') IS NOT NULL THEN session_id END,
				retry_of, sequence, policy, backend, prompt, payload, steering, parts, agent_messages, compacted_context,
				context_tokens, context_window, token_usage, status, output, created_at, started_at, updated_at, reverted_at`
		case "oauth_flows":
			source = "key, nonce, verifier, expires_at, version, NULLIF(owner_id, ''), session_key"
		case "e2b_project_workspaces":
			// Older project deletion left persisted E2B bindings behind.
			// Keep the account's default binding and all existing projects.
			from += " WHERE project_id = '' OR project_id IN (SELECT id FROM projects)"
		case "session_list_revisions":
			// Rows for vanished owners are derived data left by old revisions.
			from += " WHERE owner_id IN (SELECT id FROM users)"
			suffix = " WITHOUT ROWID"
		}
		if err := db.Exec(fmt.Sprintf(`CREATE TABLE new_%s (%s)%s`, name, layout.definition, suffix)).Error; err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		if err := db.Exec(fmt.Sprintf(`INSERT INTO new_%s (%s) SELECT %s FROM %s`, name, layout.columns, source, from)).Error; err != nil {
			return fmt.Errorf("copy %s: %w", name, err)
		}
		if err := db.Exec(fmt.Sprintf("DROP TABLE %[1]s; ALTER TABLE new_%[1]s RENAME TO %[1]s", name)).Error; err != nil {
			return fmt.Errorf("replace %s: %w", name, err)
		}
	}
	for _, definition := range definitions {
		if err := db.Exec(definition).Error; err != nil {
			return err
		}
	}
	if err := db.Exec(`DROP TABLE IF EXISTS deleted_accounts;
		CREATE INDEX idx_sessions_owner_project_ref ON agent_sessions (owner_id, project_ref)`).Error; err != nil {
		return err
	}
	return initSessionListRevisions(db)
}
