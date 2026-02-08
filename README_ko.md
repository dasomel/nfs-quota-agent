# NFS Quota Agent

[English](README.md) | 한국어

NFS 기반 PersistentVolume에 대해 파일시스템 프로젝트 쿼타를 자동으로 적용하는 Kubernetes 에이전트입니다. NFS 서버 노드에서 실행되며 파일시스템 레벨에서 스토리지 제한을 적용합니다. **XFS**와 **ext4** 파일시스템을 모두 지원합니다.

## 개요

Kubernetes에서 NFS 기반 스토리지([csi-driver-nfs](https://github.com/kubernetes-csi/csi-driver-nfs) 또는 [nfs-subdir-external-provisioner](https://github.com/kubernetes-sigs/nfs-subdir-external-provisioner) 등)를 사용할 때, PersistentVolumeClaim에 정의된 스토리지 쿼타는 파일시스템 레벨에서 적용되지 않습니다. 이 에이전트는 다음과 같은 방식으로 이 문제를 해결합니다:

1. 클러스터의 NFS PersistentVolume 감시
2. PV 용량에 기반한 프로젝트 쿼타 자동 적용 (XFS, ext4 지원)
3. PV 어노테이션을 통한 쿼타 상태 추적

## 사전 요구사항

- Kubernetes 클러스터 (v1.20+)
- **XFS** 또는 **ext4** 파일시스템의 NFS 서버
- NFS export 파일시스템에 프로젝트 쿼타 활성화
- 에이전트는 반드시 NFS 서버 노드에서 실행

### 지원 파일시스템

| 파일시스템 | 쿼타 도구 | 마운트 옵션 | 최소 커널 버전 |
|------------|-----------|-------------|----------------|
| XFS | `xfs_quota` | `prjquota` | 2.6+ |
| ext4 | `setquota` | `prjquota` | 4.5+ |

### 프로젝트 쿼타 활성화

#### XFS 파일시스템

```bash
# 현재 마운트 옵션 확인
mount | grep xfs

# prjquota 옵션으로 재마운트 (또는 /etc/fstab에 추가)
mount -o remount,prjquota /data
```

영구 설정을 위해 `/etc/fstab`에 추가:
```
/dev/sdb1  /data  xfs  defaults,prjquota  0 0
```

#### ext4 파일시스템

```bash
# 프로젝트 쿼타 기능 활성화 (1회성, 언마운트 필요)
umount /data
tune2fs -O project,quota /dev/sdb1
mount /data

# 또는 prjquota 옵션으로 재마운트
mount -o remount,prjquota /data
```

영구 설정을 위해 `/etc/fstab`에 추가:
```
/dev/sdb1  /data  ext4  defaults,prjquota  0 0
```

**참고:** ext4 프로젝트 쿼타는 Linux 커널 4.5+와 e2fsprogs 1.43+ 버전이 필요합니다.

## 설치

### 1. NFS 서버 노드에 레이블 추가

```bash
kubectl label node <nfs-server-node> nfs-server=true
```

### 2. 컨테이너 이미지 빌드 및 푸시

```bash
# 로컬 빌드
make build

# Docker 이미지 빌드 및 푸시
make docker-build docker-push REGISTRY=your-registry.io VERSION=v1.0.0

# 또는 멀티 아키텍처 이미지 빌드
make docker-buildx REGISTRY=your-registry.io VERSION=v1.0.0
```

### 3. 배포 설정 업데이트

환경에 맞게 `deploy/deployment.yaml` 수정:

```yaml
args:
  - --nfs-base-path=/export          # 컨테이너 내 로컬 마운트 경로
  - --nfs-server-path=/data          # NFS 서버의 export 경로
  - --provisioner-name=cluster.local/nfs-subdir-external-provisioner
  - --sync-interval=30s
volumes:
  - name: nfs-export
    hostPath:
      path: /data                    # 서버의 NFS export 경로
```

### 4. Kubernetes에 배포

#### Kustomize 사용

```bash
kubectl apply -k deploy/

# 또는 make 사용
make deploy
```

#### Helm Chart 사용

```bash
# 기본값으로 설치
helm install nfs-quota-agent ./charts/nfs-quota-agent \
  --namespace nfs-quota-agent \
  --create-namespace

# 커스텀 값으로 설치
helm install nfs-quota-agent ./charts/nfs-quota-agent \
  --namespace nfs-quota-agent \
  --create-namespace \
  --set config.nfsBasePath=/export \
  --set config.nfsServerPath=/data \
  --set nfsExport.hostPath=/data \
  --set config.provisionerName=cluster.local/nfs-subdir-external-provisioner

# 커스텀 values 파일로 설치
helm install nfs-quota-agent ./charts/nfs-quota-agent \
  --namespace nfs-quota-agent \
  --create-namespace \
  -f my-values.yaml

# 업그레이드
helm upgrade nfs-quota-agent ./charts/nfs-quota-agent \
  --namespace nfs-quota-agent

# 삭제
helm uninstall nfs-quota-agent -n nfs-quota-agent
```

#### Helm Chart Values

| 키 | 기본값 | 설명 |
|----|--------|------|
| `image.repository` | `nfs-quota-agent` | 이미지 저장소 |
| `image.tag` | `""` (appVersion) | 이미지 태그 |
| `config.nfsBasePath` | `/export` | 컨테이너 내 마운트 경로 |
| `config.nfsServerPath` | `/data` | NFS 서버 export 경로 |
| `config.provisionerName` | `cluster.local/nfs-subdir-external-provisioner` | 필터링할 프로비저너 |
| `config.processAllNFS` | `false` | 모든 NFS PV 처리 여부 |
| `config.syncInterval` | `30s` | 동기화 주기 |
| `nfsExport.hostPath` | `/data` | NFS export 호스트 경로 |
| `nodeSelector` | `nfs-server: "true"` | 노드 셀렉터 |
| `resources.limits.memory` | `128Mi` | 메모리 제한 |
| `resources.limits.cpu` | `100m` | CPU 제한 |

## 설정

### 명령줄 플래그

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--kubeconfig` | (클러스터 내부) | kubeconfig 파일 경로 |
| `--nfs-base-path` | `/export` | 컨테이너 내 NFS export 마운트 로컬 경로 |
| `--nfs-server-path` | `/data` | NFS 서버의 export 경로 |
| `--provisioner-name` | `cluster.local/nfs-subdir-external-provisioner` | PV 필터링용 프로비저너 이름 (csi-driver-nfs는 `nfs.csi.k8s.io`) |
| `--process-all-nfs` | `false` | 프로비저너 무관하게 모든 NFS PV 처리 |
| `--sync-interval` | `30s` | 쿼타 동기화 주기 |

### PV 어노테이션

에이전트는 PersistentVolume에 다음 어노테이션을 사용합니다:

| 어노테이션 | 설명 |
|------------|------|
| `nfs.io/project-name` | XFS 쿼타용 커스텀 프로젝트 이름 (미설정 시 자동 생성) |
| `nfs.io/quota-status` | 쿼타 상태: `pending`, `applied`, 또는 `failed` |

## 동작 원리

1. **파일시스템 감지**: 시작 시 파일시스템 타입(XFS 또는 ext4) 자동 감지

2. **PV 감지**: 다음 조건의 NFS PersistentVolume 감시:
   - `Bound` 상태
   - 설정된 프로비저너에 의해 프로비저닝됨 (`--process-all-nfs` 설정 시 모든 NFS PV)

3. **경로 매핑**: NFS 서버 경로를 로컬 경로로 변환:
   - NFS 경로: `/data/namespace-pvc-xxx`
   - 로컬 경로: `/export/namespace-pvc-xxx`

4. **프로젝트 ID 생성**: FNV 해시를 사용하여 PV 이름에서 고유한 프로젝트 ID 생성

5. **쿼타 적용**:
   - **XFS**: `xfs_quota`를 사용하여 프로젝트 초기화 및 블록 제한 설정
   - **ext4**: `chattr`로 프로젝트 속성 설정, `setquota`로 제한 설정
   - `projects`와 `projid` 파일에 프로젝트 항목 생성

6. **상태 추적**: 쿼타 상태를 반영하여 PV 어노테이션 업데이트

## NFS 서버 노드에서 실행해야 하는 이유

에이전트는 **반드시** NFS 서버 노드에서 실행해야 합니다. 선택 사항이 아닙니다.

### 이유

```
┌─────────────────────────────────────────────────────────────┐
│                     NFS 서버 노드                            │
│  ┌────────────────────────────────────────────────────────┐ │
│  │              nfs-quota-agent (Pod)                     │ │
│  │                                                        │ │
│  │   xfs_quota / setquota 명령어                          │ │
│  │              ↓                                         │ │
│  │   hostPath: /data  →  container: /export               │ │
│  └────────────────────────────────────────────────────────┘ │
│                          ↓                                  │
│  ┌────────────────────────────────────────────────────────┐ │
│  │          XFS/ext4 파일시스템 (/data)                    │ │
│  │   프로젝트 쿼타는 로컬 파일시스템에서만 설정 가능          │ │
│  └────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

| 제약 사항 | 설명 |
|-----------|------|
| **쿼타 명령은 로컬 전용** | `xfs_quota`와 `setquota`는 로컬 파일시스템에서만 동작 |
| **NFS 클라이언트에서 쿼타 설정 불가** | NFS 마운트를 통해서는 쿼타 명령이 작동하지 않음 |
| **직접 파일시스템 접근 필요** | 에이전트는 실제 디스크에 대한 hostPath 접근 필요 |

### 설정 (클러스터 내 NFS 서버)

NFS 서버가 Kubernetes 노드인 경우:

```yaml
# NFS 서버 노드에 레이블 추가
kubectl label node <nfs-server-node> nfs-server=true

# Deployment는 nodeSelector 사용
nodeSelector:
  nfs-server: "true"

# 볼륨은 실제 파일시스템을 마운트
volumes:
  - name: nfs-export
    hostPath:
      path: /data  # 서버의 실제 NFS export 디렉토리
```

### 필수 볼륨 마운트

에이전트가 쿼타 명령을 올바르게 실행하려면 특정 볼륨 마운트와 호스트 접근이 필요합니다:

| 볼륨/설정 | 경로 | 타입 | 설명 |
|-----------|------|------|------|
| `hostPID` | - | `true` | 쿼타 명령이 프로세스 정보에 접근하기 위해 필요 |
| `nfs-export` | 호스트 NFS 경로 | Directory | 실제 NFS export 디렉토리 |
| `dev` | `/dev` | Directory | 쿼타 명령을 위한 블록 디바이스 접근 |
| `etc-projects` | `/etc/projects` | FileOrCreate | 프로젝트 ID와 경로 매핑 파일 |
| `etc-projid` | `/etc/projid` | FileOrCreate | 프로젝트 이름과 ID 매핑 파일 |

Helm 차트는 이러한 마운트를 자동으로 구성합니다. 수동 배포 시:

```yaml
spec:
  hostPID: true
  containers:
    - name: nfs-quota-agent
      volumeMounts:
        - name: nfs-export
          mountPath: /export
        - name: dev
          mountPath: /dev
        - name: etc-projects
          mountPath: /etc/projects
        - name: etc-projid
          mountPath: /etc/projid
  volumes:
    - name: nfs-export
      hostPath:
        path: /data  # NFS export 경로
        type: Directory
    - name: dev
      hostPath:
        path: /dev
        type: Directory
    - name: etc-projects
      hostPath:
        path: /etc/projects
        type: FileOrCreate
    - name: etc-projid
      hostPath:
        path: /etc/projid
        type: FileOrCreate
```

**참고:** `/etc/projects`와 `/etc/projid` 파일은 XFS 및 ext4 프로젝트 쿼타 시스템에서 프로젝트 ID와 관련 경로를 추적하는 데 사용됩니다.

### 외부 NFS 서버 (클러스터 외부)

NFS 서버가 Kubernetes 클러스터의 일부가 **아닌** 경우 (예: NAS 어플라이언스, 외부 VM):

#### 옵션 1: NFS 서버에서 직접 에이전트 실행 (권장)

```bash
# NFS 서버에서 (Kubernetes 외부)

# 바이너리 다운로드 및 실행
./nfs-quota-agent \
  --kubeconfig=/path/to/kubeconfig \
  --nfs-base-path=/data \
  --nfs-server-path=/data \
  --sync-interval=30s

# 또는 systemd 서비스로 실행
cat <<EOF | sudo tee /etc/systemd/system/nfs-quota-agent.service
[Unit]
Description=NFS Quota Agent
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/nfs-quota-agent \
  --kubeconfig=/etc/kubernetes/admin.conf \
  --nfs-base-path=/data \
  --nfs-server-path=/data
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl enable --now nfs-quota-agent
```

#### 옵션 2: NFS 서버에서 Docker 컨테이너로 실행

```bash
# NFS 서버에서
docker run -d \
  --name nfs-quota-agent \
  --privileged \
  -v /data:/export \
  -v /path/to/kubeconfig:/kubeconfig:ro \
  nfs-quota-agent:latest \
  --kubeconfig=/kubeconfig \
  --nfs-base-path=/export \
  --nfs-server-path=/data
```

#### 옵션 3: NFS 서버를 클러스터에 추가

NFS 서버를 Kubernetes 노드(워커)로 추가한 후 `nodeSelector` 사용.

```bash
# NFS 서버에서 - 클러스터 조인
kubeadm join <control-plane>:6443 --token <token> ...

# 노드에 레이블 추가
kubectl label node <nfs-server> nfs-server=true
```

## 아키텍처

```
┌─────────────────┐     ┌─────────────────────────────────────────────────┐
│   Kubernetes    │     │              NFS 서버 노드                       │
│    API 서버     │     │  ┌─────────────────────────────────────────────┐│
│                 │     │  │           nfs-quota-agent                   ││
│  ┌───────────┐  │     │  │  ┌───────────┐    ┌─────────────────────┐   ││
│  │    PV     │◄─┼─────┼──┼──│  Watcher  │    │  XFS Quota Manager  │   ││
│  │ (NFS 타입)│  │     │  │  └───────────┘    └─────────────────────┘   ││
│  └───────────┘  │     │  │         │                    │              ││
│                 │     │  │         ▼                    ▼              ││
└─────────────────┘     │  │  ┌─────────────────────────────────────┐    ││
                        │  │  │           xfs_quota                 │    ││
                        │  │  └─────────────────────────────────────┘    ││
                        │  └─────────────────────────────────────────────┘│
                        │                      │                          │
                        │                      ▼                          │
                        │  ┌──────────────────────────────────────────┐   │
                        │  │      XFS 파일시스템 (/data)               │   │
                        │  │  ┌──────────┐ ┌──────────┐ ┌──────────┐  │   │
                        │  │  │ ns-pvc-1 │ │ ns-pvc-2 │ │ ns-pvc-3 │  │   │
                        │  │  │ quota:1G │ │ quota:5G │ │quota:10G │  │   │
                        │  │  └──────────┘ └──────────┘ └──────────┘  │   │
                        │  └──────────────────────────────────────────┘   │
                        └─────────────────────────────────────────────────┘
```

## CLI 명령어

에이전트는 여러 관리 명령어를 제공합니다:

```bash
# 쿼타 적용 에이전트 실행 (기본)
nfs-quota-agent run --nfs-base-path=/export --provisioner-name=nfs.csi.k8s.io

# 쿼타 상태 및 디스크 사용량 조회
nfs-quota-agent status --path=/data

# 사용량 상위 디렉토리 조회
nfs-quota-agent top --path=/data -n 10

# 실시간 모니터링 (5초마다 갱신)
nfs-quota-agent top --path=/data --watch

# 다양한 형식의 리포트 생성
nfs-quota-agent report --path=/data --format=json
nfs-quota-agent report --path=/data --format=yaml --output=report.yaml
nfs-quota-agent report --path=/data --format=csv --output=quotas.csv

# 고아 쿼타 정리 (기본: dry-run)
nfs-quota-agent cleanup --path=/data --kubeconfig=~/.kube/config

# 실제 삭제 실행
nfs-quota-agent cleanup --path=/data --kubeconfig=~/.kube/config --dry-run=false

# 확인 없이 강제 삭제
nfs-quota-agent cleanup --path=/data --kubeconfig=~/.kube/config --dry-run=false --force

# 웹 UI 대시보드 실행
nfs-quota-agent ui --path=/data --addr=:8080
```

### 웹 UI 대시보드

시각적 모니터링을 위한 웹 UI 실행:

```bash
nfs-quota-agent ui --path=/data --addr=:8080
```

브라우저에서 http://localhost:8080 접속

기능:
- 실시간 디스크 사용량 개요
- 디렉토리별 쿼타 상태 (시각적 프로그레스 바)
- 경고/초과 상태 표시
- 디렉토리 검색 및 필터
- 10초마다 자동 갱신

### 출력 예시

**status 명령어:**
```
NFS Quota Status
================

Path:       /data
Filesystem: xfs
Total:      1.0 TiB
Used:       650.5 GiB (63.5%)
Available:  373.5 GiB

Directory Quotas (45 total)
--------------------------------------------------------------------------------
DIRECTORY                  USED      QUOTA     USED%   STATUS
default-pvc-abc123         8.5 GiB   10 GiB    85.0%   OK
prod-data-xyz789           9.8 GiB   10 GiB    98.0%   WARNING
dev-logs-def456            5.2 GiB   5 GiB     104.0%  EXCEEDED
...
```

**cleanup 명령어:**
```
NFS Quota Cleanup
=================

Path: /data
Mode: DRY-RUN (no changes)

Found 25 NFS PersistentVolumes in cluster

Found 3 orphaned quotas:

PROJECT_ID   PROJECT_NAME              PATH                                     STATUS
------------------------------------------------------------------------------------------
1234567890   pv_old_namespace_pvc_xxx  /data/old-namespace-pvc-xxx              dir missing
2345678901   pv_deleted_app_data       /data/deleted-app-data                   dir exists (2.5 GiB)
3456789012   pv_test_volume            /data/test-volume                        dir missing

Dry-run mode: No changes made.
Run with --dry-run=false to remove orphaned quotas.
```

**top 명령어:**
```
NFS Quota Top - 15:04:05
Path: /data | Total: 1.0 TiB | Used: 650.5 GiB (63.5%) | Free: 373.5 GiB

#   DIRECTORY              USED      QUOTA     USED%   BAR
1   prod-data-xyz789       9.8 GiB   10 GiB    98.0%   [██████████████████░░]!
2   default-pvc-abc123     8.5 GiB   10 GiB    85.0%   [█████████████████░░░]
3   dev-logs-def456        5.2 GiB   5 GiB     104.0%  [████████████████████]!
```

### Prometheus 메트릭

에이전트는 `:9090/metrics`에서 메트릭을 제공합니다:

```
# 디스크 메트릭
nfs_disk_total_bytes{path="/data"} 1099511627776
nfs_disk_used_bytes{path="/data"} 698488954880
nfs_disk_available_bytes{path="/data"} 401022672896

# 디렉토리별 쿼타 메트릭
nfs_quota_used_bytes{directory="prod-data-xyz789"} 10523566080
nfs_quota_limit_bytes{directory="prod-data-xyz789"} 10737418240
nfs_quota_used_percent{directory="prod-data-xyz789"} 98.01

# 요약 메트릭
nfs_quota_directories_total 45
nfs_quota_warning_count 3
nfs_quota_exceeded_count 1
```

## 사용 예시

### csi-driver-nfs 사용 (권장)

[csi-driver-nfs](https://github.com/kubernetes-csi/csi-driver-nfs)는 Kubernetes에서 권장하는 NFS 프로비저너입니다. CSI 표준을 따르며 VolumeSnapshot을 지원합니다.

```bash
# csi-driver-nfs용 에이전트 설정
helm install nfs-quota-agent ./charts/nfs-quota-agent \
  --namespace nfs-quota-agent \
  --create-namespace \
  --set config.provisionerName=nfs.csi.k8s.io
```

```yaml
# csi-driver-nfs용 StorageClass
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: nfs-csi
provisioner: nfs.csi.k8s.io
parameters:
  server: nfs-server.example.com
  share: /data
reclaimPolicy: Delete
volumeBindingMode: Immediate
mountOptions:
  - nfsvers=4.1
---
# csi-driver-nfs StorageClass를 사용하는 PVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: nfs-csi
  resources:
    requests:
      storage: 10Gi  # 10Gi로 쿼타 적용
```

### nfs-subdir-external-provisioner 사용 (레거시)

```yaml
# nfs-subdir-external-provisioner StorageClass를 사용하는 PVC
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: nfs-client
  resources:
    requests:
      storage: 10Gi  # 10Gi로 쿼타 적용
```

### 정적 NFS PV 사용

수동 생성 NFS PV를 처리하려면 `--process-all-nfs` 플래그 활성화:

```yaml
# 정적 NFS PV
apiVersion: v1
kind: PersistentVolume
metadata:
  name: my-nfs-pv
spec:
  capacity:
    storage: 50Gi  # 50Gi로 쿼타 적용
  accessModes:
    - ReadWriteMany
  nfs:
    server: nfs-server.example.com
    path: /data/my-volume
```

### 쿼타 상태 확인

```bash
# PV 어노테이션 확인
kubectl get pv <pv-name> -o jsonpath='{.metadata.annotations.nfs\.io/quota-status}'

# NFS 서버에서 쿼타 확인 (XFS)
xfs_quota -x -c "report -p -h" /data

# NFS 서버에서 쿼타 확인 (ext4)
repquota -P -s /data
```

### 쿼타 초과 시 에러

Pod가 스토리지 쿼타를 초과하면 파일시스템 레벨에서 다음과 같은 에러가 발생합니다:

**XFS:**
```bash
$ dd if=/dev/zero of=/data/testfile bs=1M count=2000
dd: error writing '/data/testfile': No space left on device
```

**ext4:**
```bash
$ dd if=/dev/zero of=/data/testfile bs=1M count=2000
dd: error writing '/data/testfile': Disk quota exceeded
```

**애플리케이션 로그:**
```
Error: write /data/output.log: disk quota exceeded
```

| 상황 | errno | 메시지 |
|------|-------|--------|
| 블록 쿼타 초과 | `EDQUOT` (122) | Disk quota exceeded |
| 공간 없음 (XFS) | `ENOSPC` (28) | No space left on device |

**쿼타 적용 테스트:**
```bash
# Pod 내에서 (10Gi PVC 가정)
kubectl exec -it my-pod -- dd if=/dev/zero of=/data/test bs=1M count=15000
# 약 10GB 쓰기 후 에러 발생
```

## 개발

### 빌드

```bash
# 바이너리 빌드
make build

# Linux용 빌드 (여러 아키텍처)
make build-linux

# 테스트 실행
make test

# 커버리지 포함 테스트
make test-coverage

# 코드 포맷팅
make fmt

# go vet 실행
make vet

# 린터 실행 (golangci-lint 필요)
make lint
```

### 로컬 테스트

```bash
# kubeconfig로 로컬 실행
./bin/nfs-quota-agent \
  --kubeconfig=$HOME/.kube/config \
  --nfs-base-path=/mnt/nfs \
  --nfs-server-path=/data \
  --sync-interval=10s
```

## 문제 해결

### 쿼타가 적용되지 않는 경우

1. 파일시스템 타입과 쿼타 상태 확인:
   ```bash
   # XFS의 경우
   xfs_quota -x -c "state" /data

   # ext4의 경우
   quotaon -p /data
   tune2fs -l /dev/sdb1 | grep -i project
   ```

2. 에이전트 로그 확인:
   ```bash
   kubectl logs -n nfs-quota-agent deployment/nfs-quota-agent
   ```

3. PV 어노테이션에서 오류 확인:
   ```bash
   kubectl get pv -o jsonpath='{range .items[*]}{.metadata.name}: {.metadata.annotations.nfs\.io/quota-status}{"\n"}{end}'
   ```

### 권한 거부 오류

에이전트는 쿼타 명령 실행을 위해 privileged 접근이 필요합니다:
- 컨테이너에 `privileged: true` 보안 컨텍스트 설정
- NFS export 디렉토리가 올바르게 마운트되었는지 확인

### ext4 관련 이슈

1. 프로젝트 쿼타 기능 활성화 확인:
   ```bash
   tune2fs -l /dev/sdb1 | grep "Filesystem features"
   # project가 포함되어야 함
   ```

2. 쿼타가 마운트되었는지 확인:
   ```bash
   mount | grep prjquota
   ```

3. e2fsprogs 버전 확인 (1.43+ 필요):
   ```bash
   tune2fs -V
   ```

## 라이선스

Apache License, Version 2.0 하에 라이선스됩니다. 자세한 내용은 [LICENSE](LICENSE)를 참조하세요.

## 기여

기여를 환영합니다! 이슈와 풀 리퀘스트를 자유롭게 제출해 주세요.
