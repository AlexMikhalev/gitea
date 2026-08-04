# Gitea Robot API Security Documentation

**Version**: 1.0.0  
**Date**: 2026-04-08  
**Status**: Production Ready  

---

## Overview

The Gitea Robot API provides secure, agent-optimized access to issue data with PageRank-based prioritization. This document describes the security model, configuration, and best practices.

---

## Security Model

### Authentication

All Robot API endpoints require authentication via:
- **Session cookies** (for web UI users)
- **Access tokens** (for API clients and agents)

Unauthenticated requests to private repositories receive a **404 Not Found** response to prevent repository enumeration.

### Authorization

The Robot API enforces repository-level permissions:

| Permission | Requirement | Behavior on Denial |
|------------|-------------|-------------------|
| Read Issues | `unit.TypeIssues` read access | 404 Not Found |
| Private Repo Access | Authenticated user | 404 Not Found |

### Information Disclosure Prevention

All error conditions return **404 Not Found** to prevent:
- Repository enumeration attacks
- Username enumeration
- Repository existence leaks

Examples:
- Invalid owner/repo → 404
- Repository doesn't exist → 404
- Permission denied → 404
- Validation failure → 404

---

## Rate Limiting & Caching

### PageRank Cache

PageRank calculations are cached to prevent CPU exhaustion:

| Setting | Default | Description |
|---------|---------|-------------|
| `PAGERANK_CACHE_TTL` | 300s (5 min) | Cache expiration time |
| Concurrent computation | Prevented | Only one calculation per repo at a time |

**Behavior**:
- Cache hit → Return cached scores immediately
- Cache miss, not computing → Start calculation, cache results
- Cache miss, already computing → Return "in progress" error

### Configuration

```ini
[issue_graph]
ENABLED = true
PAGERANK_CACHE_TTL = 300
AUDIT_LOG = true
STRICT_MODE = false
ROOM_HOOK_SECRET =
```

| Setting | Default | Description |
|---------|---------|-------------|
| `ENABLED` | `true` | Master switch for the issue graph feature |
| `PAGERANK_CACHE_TTL` | `300` | PageRank cache expiration, seconds |
| `AUDIT_LOG` | `true` | Write `[ROBOT_AUDIT]` records for Robot API access |
| `STRICT_MODE` | `false` | Return 404 on any error, never 500 |
| `ROOM_HOOK_SECRET` | *(empty)* | Master secret for the branch-as-room webhook. **Empty disables the route** — see below |

---

## Branch-as-room Webhook (`POST /api/v1/robot/room/hook`)

The branch-as-room automation (issue #56) receives Gitea webhook deliveries and opens, comments on,
and closes one "room" issue per `feat/*` branch. It is the only Robot API route that **writes**, and
the only one authenticated by a signature rather than a token.

### Enabling it

```ini
[issue_graph]
ROOM_HOOK_SECRET = <long random string, e.g. `openssl rand -hex 32`>
```

**An empty or missing `ROOM_HOOK_SECRET` disables the route: it answers 404, exactly as if it did not
exist.** This is deliberate — a disabled route must not be distinguishable from an absent one — but it
also means a typo in the key name silently turns the feature off rather than failing loudly. If
deliveries return 404, check the spelling of the key and the `[issue_graph]` section header first.

### Per-repository secrets (do not hand out `ROOM_HOOK_SECRET`)

`ROOM_HOOK_SECRET` is a **master secret and never goes into a webhook configuration**. Each
repository's webhook secret is derived from it:

```
repo_secret = HMAC-SHA256(ROOM_HOOK_SECRET, "gitea-robot/room:v1:<owner>/<repo>")   # lower-case owner and repo, hex output
```

```bash
# The webhook secret to configure on acme/project:
printf 'gitea-robot/room:v1:acme/project' | openssl dgst -sha256 -mac HMAC -macopt "key:$ROOM_HOOK_SECRET" -r | cut -d' ' -f1
```

A delivery is verified with the secret of the repository **it names in its payload**, so:

- A repo admin can be given their own repository's derived secret without being able to forge
  deliveries for any other repository — computing another repository's secret needs the master.
- The master secret itself never verifies a delivery, and stays inside `app.ini` and the site-admin
  trust boundary.
- Rotating the master secret invalidates every repository's derived secret at once; rotate one
  repository by moving it to a new master only if you are prepared to reconfigure them all.

Configure the derived value as the **Secret** of a repository webhook pointing at
`https://<instance>/api/v1/robot/room/hook`, with the `Push`, `Delete`, `Pull Request` and
`Repository status` events enabled.

### Authorization of writes

A valid signature authenticates the *repository*, never a user. The `sender` in the payload is
therefore treated as an attribution preference, not a credential:

| Step | Rule | On failure |
|------|------|------------|
| Signature | HMAC-SHA256 over the raw body, in `X-Gitea-Signature` (raw hex) or `X-Hub-Signature-256` (`sha256=` prefixed) | 401 |
| Repository | The repository named in the payload (the same one whose secret verified the delivery) | 404 |
| Actor | `sender`, else the repository owner; must be an active individual account | 403 |
| Permission | The actor needs **write access to issues** on that repository | 403 |

There is no site-admin fallback: an organization-owned repository whose delivery names no eligible
actor is refused (403) rather than having its rooms authored by whichever site admin has the lowest
user id.

### Anonymous API access is required

The delivery carries no token. On instances with strict sign-in
(`[service] REQUIRE_SIGNIN_VIEW = true`), tokenless API requests are rejected with **403 before the
HMAC check runs** (`routers/api/v1/api.go`, `verifyAuthWithOptions`). The room hook only works where
anonymous API access is allowed. If every delivery fails with 403 and no `[ROBOT_AUDIT]` record
appears, this is the cause — the request never reached the handler.

### Audit records

Both outcomes are logged, so a caller probing for repository names is visible:

```
[ROBOT_AUDIT] status=DENIED  user=webhook(uid=0) repo=acme/project endpoint=/api/v1/robot/room/hook ... reason=bad_signature
[ROBOT_AUDIT] status=DENIED  user=webhook(uid=0) repo=acme/project endpoint=/api/v1/robot/room/hook ... reason=repo_not_found
[ROBOT_AUDIT] status=DENIED  user=webhook(uid=0) repo=acme/project endpoint=/api/v1/robot/room/hook ... reason=actor_denied
[ROBOT_AUDIT] status=SUCCESS user=alice(uid=7)   repo=acme/project endpoint=/api/v1/robot/room/hook ...
```

---

## Audit Logging

All Robot API access is logged with:

```
[ROBOT_AUDIT] status=SUCCESS|DENIED user=username(uid=N) repo=owner/repo endpoint=/api/v1/robot/triage ip=x.x.x.x timestamp=RFC3339 [reason=...]
```

**Log Levels**:
- Successful access → INFO
- Denied access → INFO (with reason)
- Errors → ERROR

**Enable/Disable**:
```ini
[issue_graph]
AUDIT_LOG = true   # Enable
AUDIT_LOG = false  # Disable
```

---

## API Endpoints

### GET /api/v1/robot/triage

Returns PageRank-scored issues for prioritization.

**Parameters**:
- `owner` (required): Repository owner
- `repo` (required): Repository name

**Response** (200 OK):
```json
{
  "quick_ref": {
    "total": 150,
    "open": 42
  },
  "recommendations": [
    {
      "id": 123,
      "index": 42,
      "title": "Fix critical bug",
      "pagerank": 0.0852,
      "status": "open"
    }
  ],
  "project_health": {
    "avg_pagerank": 0.0234,
    "max_pagerank": 0.0852
  }
}
```

**Error Responses**:
- 404: Feature disabled, repo not found, permission denied, or validation error
- 500: Internal server error (rare, also returns 404 in strict mode)

### GET /api/v1/robot/ready

Returns issues ready to be worked on (no blocking dependencies).

**Parameters**:
- `owner` (required): Repository owner
- `repo` (required): Repository name

**Response** (200 OK):
```json
{
  "repo_id": 123,
  "repo_name": "myrepo",
  "total_count": 10,
  "ready_issues": [
    {
      "id": 456,
      "index": 23,
      "title": "Add feature X",
      "page_rank": 0.0456,
      "priority": 25,
      "is_blocked": false,
      "blocker_count": 0
    }
  ]
}
```

### GET /api/v1/robot/graph

Returns the full dependency graph for visualization.

**Parameters**:
- `owner` (required): Repository owner
- `repo` (required): Repository name

**Response** (200 OK):
```json
{
  "repo_id": 123,
  "repo_name": "myrepo",
  "node_count": 50,
  "edge_count": 30,
  "nodes": [...],
  "edges": [...]
}
```

---

## Security Best Practices

### 1. Use Access Tokens

For API clients, use personal access tokens with minimal required scopes:

```bash
curl -H "Authorization: token YOUR_TOKEN" \
  "https://gitea.example.com/api/v1/robot/triage?owner=acme&repo=project"
```

### 2. Enable Audit Logging

Always enable audit logging in production:

```ini
[issue_graph]
AUDIT_LOG = true
```

### 3. Configure Cache TTL Appropriately

Balance freshness vs. performance:

| Use Case | Recommended TTL |
|----------|----------------|
| High-activity repos | 60-120 seconds |
| Normal repos | 300 seconds (default) |
| Low-activity repos | 600-1800 seconds |

### 4. Monitor Cache Stats

Check cache effectiveness:

```go
cache := robot.GetPageRankCache()
entries, computing := cache.GetStats()
log.Info("PageRank cache: %d entries, %d computing", entries, computing)
```

### 5. Use Strict Mode for High-Security Environments

```ini
[issue_graph]
STRICT_MODE = true
```

In strict mode, all errors return 404 (no 500 errors ever exposed).

---

## Input Validation

All inputs are validated for:

| Check | Rule |
|-------|------|
| Owner name | Required, max 40 chars |
| Repo name | Required, max 100 chars |
| Path traversal | `..` blocked |
| Invalid chars | `/\<>:|?*\x00` blocked |

**Note**: Validation failures return 404 to prevent information disclosure.

---

## Deployment Checklist

Before deploying to production:

- [ ] Enable issue graph feature
- [ ] Configure appropriate cache TTL
- [ ] Enable audit logging
- [ ] Test with private repository (verify 404 for unauthorized access)
- [ ] Test with public repository (verify 200 for anonymous access)
- [ ] Verify PageRank caching works (second request should be faster)
- [ ] Check audit logs are being written
- [ ] Configure log rotation for audit logs
- [ ] If using branch-as-room: set `ROOM_HOOK_SECRET`, verify the route no longer answers 404
- [ ] If using branch-as-room: configure each repository's webhook with its **derived** secret, never the master
- [ ] If using branch-as-room: confirm anonymous API access is allowed (`REQUIRE_SIGNIN_VIEW` not strict)

---

## Troubleshooting

### Issue: 404 for all requests

**Causes**:
1. Feature disabled → Check `ENABLED = true`
2. Permission denied → Verify user has issue read access
3. Repo doesn't exist → Check owner/repo spelling

### Issue: PageRank recalculation on every request

**Causes**:
1. Cache TTL too low → Increase `PAGERANK_CACHE_TTL`
2. Cache not persisting → Check for errors in logs

### Issue: Slow response times

**Solutions**:
1. Increase cache TTL
2. Check database performance
3. Monitor concurrent computation (should be 1 per repo)

### Issue: Audit logs not appearing

**Solutions**:
1. Verify `AUDIT_LOG = true`
2. Check log level (must be INFO or lower)
3. Verify log destination is writable

---

## Security Changelog

| Version | Date | Changes |
|---------|------|---------|
| 1.0.0 | 2026-04-08 | Initial secure implementation with auth, audit, caching |
| 1.1.0 | 2026-08-04 | Branch-as-room webhook: per-repository derived secrets, actor permission check, failure-path audit records |

---

## References

- [Gitea Permissions Documentation](https://docs.gitea.io/en-us/permissions/)
- [PageRank Algorithm](https://en.wikipedia.org/wiki/PageRank)
- [Gitea API Documentation](https://try.gitea.io/api/swagger)

---

**Document Maintainer**: Terraphim Security Team  
**Last Updated**: 2026-04-08T17:47:00Z
