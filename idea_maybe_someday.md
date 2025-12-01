# Feature Proposal: Atomic Symlink Deployments ("Capistrano Style")

## 1. The Problem
The current "Hot/Cold" deployment strategy overwrites files in the target directory. This creates several risks:
1.  **Mixed Assets:** If a deploy fails or network cuts out, the directory might contain a mix of Version A binary and Version B templates/assets, causing crashes.
2.  **Slow Rollbacks:** Rolling back requires moving backup files and re-copying, which is slow and can fail.
3.  **Dirty State:** Over time, the deployment directory accumulates junk files that were removed from the repo but remain on the server.

## 2. The Solution
Adopt a **Symlink-based release strategy** (popularized by Capistrano/Ruby on Rails). We separate the **Code** (Immutable Releases) from the **Data** (Persistent Shared State).

### 2.1 Directory Structure
The structure on the Remote VPS will look like this:

```text
/home/deploy_user/web/my-app/
├── current -> releases/v1.0.1  # Symlink to the active version
├── releases/
│   ├── v1.0.0/                 # Old Release (kept for rollback)
│   ├── v1.0.1/                 # Active Release
│   └── v2.0.0-rc1/             # Being deployed right now...
└── shared/
    ├── data/                   # Persistent SQLite DB, Uploads
    └── .env                    # Secrets file
```

### 2.2 The Deployment Lifecycle
When running `deploy release v2.0.0 prod`:

1.  **Setup:** Create directory `releases/v2.0.0`.
2.  **Upload:** Rsync binary, assets, templates into `releases/v2.0.0`.
3.  **Link Shared:** Create symlinks inside `releases/v2.0.0` pointing to `../../shared`:
    *   `ln -s ../../shared/.env .env`
    *   `ln -s ../../shared/data data`
4.  **Build:** Run `podman build` inside `releases/v2.0.0`.
5.  **Stop & Swap (Atomic Switch):**
    *   Stop Service: `systemctl --user stop my-app`
    *   Update Symlink: `ln -sfn releases/v2.0.0 current`
    *   Start Service: `systemctl --user start my-app`
6.  **Health Check:** Curl the health endpoint.
7.  **Cleanup:** If successful, delete old releases (keep last 5).

### 2.3 The Rollback Mechanism
If the Health Check fails, or if a user manually runs `deploy rollback`:

1.  Identify the previous version (e.g., `v1.0.1`).
2.  Update Symlink: `ln -sfn releases/v1.0.1 current`.
3.  Restart Service.
*Result:* Instant restoration of the previous binary AND its matching assets. No file copying required.

## 3. Database & Maintenance Mode
Since we use SQLite, we cannot have two versions running simultaneously (Blue/Green) without risking corruption.

**Strategy:**
1.  **Downtime Window:** The service is stopped during the "Swap" phase (approx. 1-3 seconds).
2.  **Traefik Handling:**
    *   We can configure a Traefik Middleware to serve a custom `503 Service Unavailable` page during this brief window.
    *   Alternatively, Traefik's retry mechanism can mask the downtime if the restart is fast enough.

## 4. Implementation Checklist

- [ ] **Config Update:** Add `keep_releases: 5` to `deploy.yaml`.
- [ ] **Remote Init:** Create `releases/` and `shared/` folders on first run.
- [ ] **Data Migration:** Helper command to move existing `./data` to `./shared/data` for existing users.
- [ ] **Deploy Logic:**
    *   Update `rsync` target to timestamped/versioned folder.
    *   Implement "Shared Linking" logic (symlinking config/data).
    *   Implement "Atomic Switch" logic.
- [ ] **Rollback Logic:** Change from "Move `.bak` file" to "Point symlink to `prev` folder".
- [ ] **Pruning:** Logic to find and `rm -rf` directories older than N releases.

## 5. Benefits
*   **Guaranteed Consistency:** Code and Assets always match.
*   **Instant Rollbacks:** < 1 second recovery time.
*   **History:** You can inspect previous builds on the server for debugging.
*   **Cleanliness:** Each release folder is a fresh start; no leftover junk files.