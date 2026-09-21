# Multi-user deployment

`ainovel-web` is a server-side Myna OAuth client. Register a public Myna OAuth
client with `profile:read token_base:read token_base:write offline_access`.

Set these deployment environment variables before starting the service:

```sh
AINOVEL_MYNA_ISSUER=https://myna.example.com
AINOVEL_MYNA_CLIENT_ID=registered-client-id
AINOVEL_MYNA_API_BASE_URL=https://gateway.example.com/v1
AINOVEL_WEB_SESSION_SECRET=base64url-at-least-32-bytes
AINOVEL_WEB_ENCRYPTION_KEY=base64url-exactly-32-bytes
AINOVEL_WEB_DATA_DIR=/var/lib/ainovel-web
AINOVEL_WEB_ADDR=127.0.0.1:4788
```

SQLite defaults to `AINOVEL_WEB_DATA_DIR/ainovel-web.sqlite`. Store the data
directory on a persistent volume. Terminate HTTPS at the reverse proxy. For a
local HTTP-only development server, set `AINOVEL_WEB_INSECURE_COOKIE=1`.

The server encrypts OAuth and Token Base credentials at rest. Collaborative
books live below `shared/books/`. Only one logged-in user can write a book at a
time; other write requests wait in FIFO order and may be cancelled by their
requester. When a queued request starts, it uses that user's own Myna Token
Base credential. Existing `users/<myna-user-id>/books/` directories are left
untouched and are not merged automatically.
