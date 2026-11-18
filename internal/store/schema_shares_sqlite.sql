CREATE TABLE IF NOT EXISTS share_template_usage_daily (
	asset_set TEXT NOT NULL,
	usage_day TEXT NOT NULL,
	share_id TEXT NOT NULL,
	first_observed_at TIMESTAMP NOT NULL,
	render_count INTEGER NOT NULL DEFAULT 0,
	resource_request_count INTEGER NOT NULL DEFAULT 0,
	last_rendered_at TIMESTAMP,
	last_resource_requested_at TIMESTAMP,
	PRIMARY KEY (asset_set, usage_day, share_id)
);

CREATE TABLE IF NOT EXISTS shares (
	id TEXT PRIMARY KEY,
	owner_user_id TEXT NOT NULL,
	conversation_id TEXT,
	kind TEXT NOT NULL DEFAULT 'conversation',
	title TEXT NOT NULL DEFAULT '历史对话分享',
	description_json BLOB,
	snapshot_version INTEGER NOT NULL,
	snapshot_json BLOB NOT NULL,
	created_at TIMESTAMP NOT NULL,
	revoked_at TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_shares_owner_created
	ON shares(owner_user_id, created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS share_access (
	share_id TEXT PRIMARY KEY,
	last_accessed_at TIMESTAMP NOT NULL,
	FOREIGN KEY (share_id) REFERENCES shares(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS share_resources (
	share_id TEXT NOT NULL,
	resource_id TEXT NOT NULL,
	name TEXT NOT NULL,
	mime_type TEXT NOT NULL,
	size_bytes INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	logical_path TEXT,
	PRIMARY KEY (share_id, resource_id),
	FOREIGN KEY (share_id) REFERENCES shares(id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_share_resource_path
	ON share_resources(share_id, logical_path);
