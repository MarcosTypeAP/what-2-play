# What 2 Play?

> ⚠️ Due to free tier, on the first request, the page may take up to 1 minute to load as the server spins up.

This page shows you what multiplayer games you and your friends share in common on Steam, so you can decide what to play. That's it.

(Steam APIs are a f-ing mess)

> This project uses a database to cache some responses from the Steam API, to avoid overload it and errors due to reaching request per minute limits.

# Requirements

- `go`
- Steam API key (https://steamcommunity.com/dev)

# Run
```bash
# Install dependencies
$ go mod tidy

# Set the environment variables
# I chose 'libsql' as the database engine ('Turso' for production)
$ cat <<EOF > .env
PORT=6969
STEAM_API_KEY=xxxxxxxxxx

# If you want a local database, use
# DB_URL=file:./local.db
DB_URL=libsql://<database-name>.turso.io
DB_TOKEN=xxxxxxxxxx
EOF

# Run production
$ go run .

# Run development
$ go run -tags='dev' .

# Run with mock server (outgoing request's host changed to 127.0.0.1:8080)
$ MOCK=1 go run .
```
