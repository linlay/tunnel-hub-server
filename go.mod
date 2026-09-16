module example.invalid/tunnel-hub-server

go 1.25.0

require (
	github.com/go-sql-driver/mysql v1.9.3
	github.com/gorilla/websocket v1.5.3
	github.com/hashicorp/yamux v0.0.0
	golang.org/x/crypto v0.45.0
)

require filippo.io/edwards25519 v1.1.0 // indirect

replace github.com/hashicorp/yamux => ./third_party/yamux
