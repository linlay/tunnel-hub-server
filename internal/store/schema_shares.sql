CREATE TABLE IF NOT EXISTS shares (
	id VARCHAR(128) PRIMARY KEY,
	owner_user_id VARCHAR(255) NOT NULL,
	conversation_id VARCHAR(255),
	kind VARCHAR(128) NOT NULL DEFAULT 'conversation',
	title VARCHAR(255) NOT NULL DEFAULT '历史对话分享',
	description_json LONGBLOB,
	snapshot_version INTEGER NOT NULL,
	snapshot_json LONGBLOB NOT NULL,
	created_at DATETIME(6) NOT NULL,
	revoked_at DATETIME(6),
	INDEX idx_shares_owner_created (owner_user_id, created_at DESC, id DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS share_access (
	share_id VARCHAR(128) PRIMARY KEY,
	last_accessed_at DATETIME(6) NOT NULL,
	FOREIGN KEY (share_id) REFERENCES shares(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS share_resources (
	share_id VARCHAR(128) NOT NULL,
	resource_id VARCHAR(80) NOT NULL,
	name VARCHAR(255) NOT NULL,
	mime_type VARCHAR(100) NOT NULL,
	size_bytes BIGINT NOT NULL,
	sha256 VARCHAR(64) NOT NULL,
	logical_path VARCHAR(255),
	PRIMARY KEY (share_id, resource_id),
	UNIQUE KEY idx_share_resource_path (share_id, logical_path),
	FOREIGN KEY (share_id) REFERENCES shares(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
