# Web UI Guide

NFS Quota Agent provides a web-based dashboard for monitoring and managing NFS quotas.

## Enabling Web UI

```bash
# CLI
nfs-quota-agent run --enable-ui --ui-addr=:8080

# Helm
helm install nfs-quota-agent ./charts/nfs-quota-agent \
  --set webUI.enabled=true \
  --set webUI.addr=":8080"
```

Access the UI at `http://<node-ip>:8080`

---

## Dashboard Overview

![Dashboard Overview](screenshots/01-dashboard-quotas.png)

The dashboard displays real-time NFS quota status with the following summary cards:

| Card | Description |
|------|-------------|
| **Total Disk** | Total disk capacity of NFS export |
| **Used** | Current disk usage with percentage |
| **Available** | Free disk space |
| **Directories** | Number of quota-managed directories |
| **Warning** | Directories using 90-99% of quota |
| **Exceeded** | Directories exceeding quota limit |

---

## Tabs

### Quotas Tab

Main quota monitoring view showing all directories with quotas.

**Features:**
- **Sortable columns**: Click any header to sort
- **Search**: Filter directories by name
- **Expandable rows**: Click a row to view directory contents
- **Usage bar**: Visual representation of quota usage
- **Status badges**: OK (green), Warning (yellow), Exceeded (red)

**Columns:**
| Column | Description |
|--------|-------------|
| Directory | Directory name (click to expand file list) |
| PV | PersistentVolume name and binding status |
| PVC | PersistentVolumeClaim name and namespace |
| Used | Current storage usage |
| Quota | Configured quota limit |
| Usage | Percentage bar with numeric value |
| Status | OK / Warning / Exceeded / No Quota |

#### File Browser

Click any row to expand and view directory contents:
- 📁 Directories shown first
- 📄 Files with size information
- Sorted alphabetically

---

### Audit Tab

![Audit Tab](screenshots/05-audit-logs.png)

View quota operation history (requires `--enable-audit`).

**Filters:**
- **Action**: CREATE, UPDATE, DELETE, CLEANUP
- **Limit**: Number of entries (50, 100, 500, 1000)
- **Fails only**: Show only failed operations

**Columns:**
| Column | Description |
|--------|-------------|
| Timestamp | Operation time |
| Action | CREATE / UPDATE / DELETE / CLEANUP |
| PV Name | Associated PersistentVolume |
| Namespace | Kubernetes namespace |
| Path | Directory path |
| Quota | Applied quota size |
| Status | Success (✓) or Fail (✗) with error |

---

### Orphans Tab

![Orphans Tab](screenshots/02-orphans.png)

Manage orphaned directories (requires `--enable-auto-cleanup`).

**Info Cards:**
- Cleanup status (Enabled/Disabled)
- Mode (Dry-Run/Live)
- Grace Period
- Orphan Count

**Features:**
- **Checkbox selection**: Select individual orphans
- **Select all**: Header checkbox for bulk selection
- **Delete Selected**: Immediately delete selected orphans (Live mode only)
- **Expandable rows**: View orphan directory contents

**Columns:**
| Column | Description |
|--------|-------------|
| ☐ | Selection checkbox (Live mode only) |
| Name | Directory name |
| Path | Full path |
| Size | Directory size |
| First Seen | When orphan was detected |
| Age | Time since first detection |
| Status | Can Delete / In Grace Period |

#### Orphan Deletion

In **Live mode** (cleanup.dryRun=false):
1. Select orphans using checkboxes
2. Click "Delete Selected" button
3. Confirm deletion in dialog
4. Orphans are immediately removed

---

### Trends Tab

![Trends Tab](screenshots/03-trends.png)

View usage history and trends (requires `--enable-history`).

**Info Cards:**
- History entries count
- Tracked paths count
- Retention period

**Columns:**
| Column | Description |
|--------|-------------|
| Directory | Directory name |
| Current | Current usage |
| Quota | Quota limit |
| 24h Change | Usage change in last 24 hours |
| 7d Change | Usage change in last 7 days |
| 30d Change | Usage change in last 30 days |
| Trend | ↑ (increasing) / ↓ (decreasing) / → (stable) |

---

### Policies Tab

![Policies Tab](screenshots/04-policies.png)

View namespace quota policies (requires `--enable-policy`).

**Displays:**
- Namespace-level quota policies
- LimitRange configurations
- ResourceQuota usage
- Policy violations

**Priority Order:**
1. LimitRange (PersistentVolumeClaim limits)
2. Namespace Annotations (`nfs.io/default-quota`, `nfs.io/max-quota`)
3. Global Default (`--default-quota`)

---

## API Endpoints

The Web UI uses the following REST APIs:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/status` | GET | Disk and quota summary |
| `/api/quotas` | GET | List all quotas |
| `/api/config` | GET | Feature flags |
| `/api/audit` | GET | Audit log entries |
| `/api/orphans` | GET | Orphan directories |
| `/api/orphans/delete` | POST | Delete orphan |
| `/api/files` | GET | Directory contents |
| `/api/history` | GET | Usage history |
| `/api/trends` | GET | Usage trends |
| `/api/policies` | GET | Namespace policies |
| `/api/violations` | GET | Policy violations |

### Example API Calls

```bash
# Get quota status
curl http://localhost:8080/api/status

# List quotas
curl http://localhost:8080/api/quotas

# Get directory contents
curl "http://localhost:8080/api/files?path=/export/default"

# Delete orphan (Live mode only)
curl -X POST http://localhost:8080/api/orphans/delete \
  -H "Content-Type: application/json" \
  -d '{"path":"/export/orphan-dir"}'
```

---

## Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `R` | Refresh data |
| `1-5` | Switch tabs |
| `/` | Focus search |

---

## Troubleshooting

### Tab not visible

Tabs appear based on enabled features:

| Tab | Required Flag |
|-----|---------------|
| Audit | `--enable-audit` |
| Orphans | `--enable-auto-cleanup` |
| Trends | `--enable-history` |
| Policies | `--enable-policy` |

### Empty quota list

1. Check if NFS path is correctly mounted
2. Verify project quota is enabled on filesystem
3. Check agent logs: `kubectl logs -n nfs-quota-agent deploy/nfs-quota-agent`

### Delete button not showing

Orphan deletion requires:
- `--enable-auto-cleanup`
- `--cleanup-dry-run=false` (Live mode)
