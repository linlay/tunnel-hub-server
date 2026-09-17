CREATE TABLE IF NOT EXISTS admin_users (
	id BIGINT PRIMARY KEY AUTO_INCREMENT,
	username VARCHAR(255) NOT NULL UNIQUE,
	password_hash VARCHAR(255) NOT NULL,
	status VARCHAR(255) NOT NULL DEFAULT 'active',
	created_at DATETIME(6) NOT NULL,
	updated_at DATETIME(6) NOT NULL,
	last_login_at DATETIME(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS admin_sessions (
	id VARCHAR(128) PRIMARY KEY,
	user_id BIGINT NOT NULL,
	session_hash VARCHAR(255) NOT NULL UNIQUE,
	expires_at DATETIME(6) NOT NULL,
	created_at DATETIME(6) NOT NULL,
	last_seen_at DATETIME(6) NOT NULL,
	FOREIGN KEY(user_id) REFERENCES admin_users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS tunnel_tokens (
	id VARCHAR(128) PRIMARY KEY,
	name LONGTEXT NOT NULL,
	token_hash VARCHAR(255) NOT NULL,
	token_prefix VARCHAR(255) NOT NULL,
	active BOOLEAN NOT NULL DEFAULT 1,
	created_at DATETIME(6) NOT NULL,
	last_used_at DATETIME(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS routes (
	id VARCHAR(128) PRIMARY KEY,
	public_host VARCHAR(255) NOT NULL UNIQUE,
	target_url LONGTEXT NOT NULL,
	token_id VARCHAR(128),
	active BOOLEAN NOT NULL DEFAULT 1,
	created_at DATETIME(6) NOT NULL,
	updated_at DATETIME(6) NOT NULL,
	FOREIGN KEY (token_id) REFERENCES tunnel_tokens(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS desktop_devices (
	device_id VARCHAR(128) PRIMARY KEY,
	display_device_id VARCHAR(255) NOT NULL,
	device_name LONGTEXT,
	owner_user_id VARCHAR(255) NOT NULL,
	owner_email LONGTEXT,
	owner_name LONGTEXT,
	public_host VARCHAR(255) NOT NULL UNIQUE,
	created_at DATETIME(6) NOT NULL,
	updated_at DATETIME(6) NOT NULL,
	UNIQUE(owner_user_id, display_device_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS desktop_webapps (
	id VARCHAR(128) PRIMARY KEY,
	device_id VARCHAR(128) NOT NULL,
	name VARCHAR(63) NOT NULL,
	route_id VARCHAR(128) NOT NULL,
	public_host VARCHAR(255) NOT NULL UNIQUE,
	target_url LONGTEXT NOT NULL,
	active BOOLEAN NOT NULL DEFAULT 1,
	created_at DATETIME(6) NOT NULL,
	updated_at DATETIME(6) NOT NULL,
	FOREIGN KEY (device_id) REFERENCES desktop_devices(device_id),
	FOREIGN KEY (route_id) REFERENCES routes(id),
	UNIQUE(device_id, name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS agent_sessions (
	id VARCHAR(128) PRIMARY KEY,
	token_id VARCHAR(128) NOT NULL,
	remote_addr LONGTEXT NOT NULL,
	connected_at DATETIME(6) NOT NULL,
	disconnected_at DATETIME(6),
	FOREIGN KEY (token_id) REFERENCES tunnel_tokens(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS desktop_sessions (
	id VARCHAR(128) PRIMARY KEY,
	device_id VARCHAR(128) NOT NULL,
	remote_addr LONGTEXT NOT NULL,
	connected_at DATETIME(6) NOT NULL,
	disconnected_at DATETIME(6),
	FOREIGN KEY (device_id) REFERENCES desktop_devices(device_id),
	INDEX idx_desktop_sessions_device_connected (device_id, connected_at DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS events (
	id BIGINT PRIMARY KEY AUTO_INCREMENT,
	type VARCHAR(255) NOT NULL,
	message LONGTEXT NOT NULL,
	details LONGTEXT NOT NULL,
	created_at DATETIME(6) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS conversation_shares (
	id VARCHAR(128) PRIMARY KEY,
	owner_user_id VARCHAR(255) NOT NULL,
	conversation_id VARCHAR(255) NOT NULL,
	snapshot_version INTEGER NOT NULL,
	snapshot_json LONGBLOB NOT NULL,
	created_at DATETIME(6) NOT NULL,
	expires_at DATETIME(6),
	revoked_at DATETIME(6),
	single_use INTEGER NOT NULL DEFAULT 0,
	CONSTRAINT chk_conversation_share_single_use CHECK (
		single_use IN (0, 1)
		AND (single_use = 0 OR expires_at IS NULL)
	),
	INDEX idx_conversation_shares_owner_created (owner_user_id, created_at DESC, id DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS conversation_share_access (
	share_id VARCHAR(128) PRIMARY KEY,
	last_accessed_at DATETIME(6) NOT NULL,
	FOREIGN KEY (share_id) REFERENCES conversation_shares(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS traffic_events (
	id BIGINT PRIMARY KEY AUTO_INCREMENT,
	object_type VARCHAR(255) NOT NULL,
	public_host VARCHAR(255) NOT NULL DEFAULT '',
	route_id VARCHAR(128),
	token_id VARCHAR(128),
	device_id VARCHAR(128),
	session_id VARCHAR(128),
	kind VARCHAR(255) NOT NULL,
	method VARCHAR(255) NOT NULL DEFAULT '',
	path LONGTEXT NOT NULL,
	status_code INTEGER NOT NULL DEFAULT 0,
	bytes_in BIGINT NOT NULL DEFAULT 0,
	bytes_out BIGINT NOT NULL DEFAULT 0,
	error LONGTEXT NOT NULL,
	occurred_at DATETIME(6) NOT NULL,
	INDEX idx_traffic_events_occurred_at (occurred_at),
	INDEX idx_traffic_events_public_host (public_host),
	INDEX idx_traffic_events_token_id (token_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
