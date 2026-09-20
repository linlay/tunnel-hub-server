CREATE TABLE IF NOT EXISTS admin_users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'active',
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL,
	last_login_at TIMESTAMP
);

CREATE TABLE IF NOT EXISTS admin_sessions (
	id TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL,
	session_hash TEXT NOT NULL UNIQUE,
	expires_at TIMESTAMP NOT NULL,
	created_at TIMESTAMP NOT NULL,
	last_seen_at TIMESTAMP NOT NULL,
	FOREIGN KEY(user_id) REFERENCES admin_users(id)
);

CREATE TABLE IF NOT EXISTS tunnel_tokens (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	token_hash TEXT NOT NULL,
	token_prefix TEXT NOT NULL,
	active BOOLEAN NOT NULL DEFAULT 1,
	created_at TIMESTAMP NOT NULL,
	last_used_at TIMESTAMP
);

CREATE TABLE IF NOT EXISTS routes (
	id TEXT PRIMARY KEY,
	public_host TEXT NOT NULL UNIQUE,
	target_url TEXT NOT NULL,
	token_id TEXT,
	active BOOLEAN NOT NULL DEFAULT 1,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL,
	FOREIGN KEY (token_id) REFERENCES tunnel_tokens(id)
);

CREATE TABLE IF NOT EXISTS desktop_devices (
	device_id TEXT PRIMARY KEY,
	display_device_id TEXT NOT NULL,
	device_name TEXT,
	owner_user_id TEXT NOT NULL,
	owner_email TEXT,
	owner_name TEXT,
	public_host TEXT NOT NULL UNIQUE,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL,
	UNIQUE(owner_user_id, display_device_id)
);

CREATE TABLE IF NOT EXISTS desktop_webapps (
	id TEXT PRIMARY KEY,
	device_id TEXT NOT NULL,
	name TEXT NOT NULL,
	route_id TEXT NOT NULL,
	public_host TEXT NOT NULL UNIQUE,
	target_url TEXT NOT NULL,
	active BOOLEAN NOT NULL DEFAULT 1,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL,
	FOREIGN KEY (device_id) REFERENCES desktop_devices(device_id),
	FOREIGN KEY (route_id) REFERENCES routes(id),
	UNIQUE(device_id, name)
);

CREATE TABLE IF NOT EXISTS agent_sessions (
	id TEXT PRIMARY KEY,
	token_id TEXT NOT NULL,
	remote_addr TEXT NOT NULL,
	connected_at TIMESTAMP NOT NULL,
	disconnected_at TIMESTAMP,
	FOREIGN KEY (token_id) REFERENCES tunnel_tokens(id)
);

CREATE TABLE IF NOT EXISTS desktop_sessions (
	id TEXT PRIMARY KEY,
	device_id TEXT NOT NULL,
	remote_addr TEXT NOT NULL,
	connected_at TIMESTAMP NOT NULL,
	disconnected_at TIMESTAMP,
	FOREIGN KEY (device_id) REFERENCES desktop_devices(device_id)
);

CREATE INDEX IF NOT EXISTS idx_desktop_sessions_device_connected
	ON desktop_sessions(device_id, connected_at DESC);

CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	type TEXT NOT NULL,
	message TEXT NOT NULL,
	details TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS conversation_shares (
	id TEXT PRIMARY KEY,
	owner_user_id TEXT NOT NULL,
	conversation_id TEXT NOT NULL,
	snapshot_version INTEGER NOT NULL,
	snapshot_json BLOB NOT NULL,
	created_at TIMESTAMP NOT NULL,
	expires_at TIMESTAMP,
	revoked_at TIMESTAMP,
	single_use INTEGER NOT NULL DEFAULT 0,
	CONSTRAINT chk_conversation_share_single_use CHECK (
		single_use IN (0, 1)
		AND (single_use = 0 OR expires_at IS NULL)
	)
);

CREATE INDEX IF NOT EXISTS idx_conversation_shares_owner_created
	ON conversation_shares(owner_user_id, created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS conversation_share_access (
	share_id TEXT PRIMARY KEY,
	last_accessed_at TIMESTAMP NOT NULL,
	FOREIGN KEY (share_id) REFERENCES conversation_shares(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS conversation_share_attachments (
	share_id TEXT NOT NULL,
	attachment_id TEXT NOT NULL,
	name TEXT NOT NULL,
	mime_type TEXT NOT NULL,
	size_bytes INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	body BLOB NOT NULL,
	PRIMARY KEY (share_id, attachment_id),
	FOREIGN KEY (share_id) REFERENCES conversation_shares(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS conversation_share_claims (
	share_id TEXT PRIMARY KEY,
	token_hash BLOB NOT NULL,
	expires_at TIMESTAMP NOT NULL,
	FOREIGN KEY (share_id) REFERENCES conversation_shares(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS traffic_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	object_type TEXT NOT NULL,
	public_host TEXT NOT NULL DEFAULT '',
	route_id TEXT,
	token_id TEXT,
	device_id TEXT,
	session_id TEXT,
	kind TEXT NOT NULL,
	method TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL,
	status_code INTEGER NOT NULL DEFAULT 0,
	bytes_in INTEGER NOT NULL DEFAULT 0,
	bytes_out INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL,
	occurred_at TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_traffic_events_occurred_at ON traffic_events(occurred_at);
CREATE INDEX IF NOT EXISTS idx_traffic_events_public_host ON traffic_events(public_host);
CREATE INDEX IF NOT EXISTS idx_traffic_events_token_id ON traffic_events(token_id);
