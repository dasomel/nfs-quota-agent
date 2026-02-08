# NFS Quota Agent - 기능 가이드

NFS Quota Agent는 Kubernetes 환경에서 NFS 스토리지 쿼터를 모니터링하고 관리하기 위한 웹 대시보드를 제공합니다. 이 가이드는 실제 테스트 환경을 기반으로 각 기능을 설명합니다.

## 사전 요구사항

- NFS CSI 드라이버가 설치된 Kubernetes 클러스터
- Helm 차트로 배포된 NFS Quota Agent
- 모든 기능 활성화 (`--enable-ui`, `--enable-audit`, `--enable-auto-cleanup`, `--enable-history`, `--enable-policy`)

---

## 1. 대시보드 & Quotas 탭

![대시보드 Quotas](screenshots/01-dashboard-quotas.png)

**Quotas** 탭은 기본 화면으로, NFS 스토리지의 실시간 현황을 보여줍니다.

### 요약 카드

| 카드 | 설명 | 예시 |
|------|------|------|
| **DISK TOTAL** | NFS 익스포트 전체 디스크 용량 | 974.6 GiB (/export) |
| **DISK USED** | 현재 디스크 사용량 (퍼센트 바 포함) | 31.8 GiB (3.3%) |
| **DISK AVAILABLE** | 남은 여유 공간과 파일시스템 유형 | 942.8 GiB, XFS |
| **TOTAL DIRECTORIES** | 쿼터가 설정된 디렉토리 수 | 4개 |

### 상태 표시

| 상태 | 조건 | 색상 |
|------|------|------|
| **OK** | 사용량 < 쿼터의 90% | 녹색 |
| **Warning** | 사용량 >= 쿼터의 90% | 노란색 |
| **Exceeded** | 사용량 >= 쿼터의 100% | 빨간색 |

### 디렉토리 쿼터 테이블

각 행은 다음 정보를 표시합니다:
- **Directory**: 디렉토리명 (클릭하면 파일 브라우저 확장)
- **PV**: PersistentVolume 이름과 바인딩 상태 (Bound 뱃지)
- **PVC**: PersistentVolumeClaim 이름과 네임스페이스
- **Used / Quota**: 현재 사용량 대비 설정된 한도
- **Usage**: 시각적 진행 바와 퍼센트 수치
- **Status**: 색상 코딩 뱃지 (OK / Warning / Exceeded / No Quota / Orphaned)

기본적으로 사용률(%)이 높은 순서로 정렬됩니다. 컬럼 헤더를 클릭하여 정렬을 변경할 수 있습니다.

### 테스트 결과 분석

- **db-backup**: 50.0 MiB / 50.0 MiB (100%) — **Exceeded** 상태, 빨간색 프로그레스 바
- **logs-storage**: 180.0 MiB / 200.0 MiB (90%) — **Warning** 상태, 노란색 프로그레스 바
- **app-data**: 45.0 MiB / 100.0 MiB (45%) — **OK** 상태, 녹색 프로그레스 바
- **staging**: PV 바인딩 없는 고아 디렉토리 — **No Quota** 상태

---

## 2. Orphans 탭

![Orphans 탭](screenshots/02-orphans.png)

**Orphans** 탭은 NFS 서버에 존재하지만 클러스터에 대응하는 PersistentVolume이 없는 디렉토리를 관리합니다.

### 설정 카드

| 카드 | 설명 |
|------|------|
| **AUTO-CLEANUP** | 자동 정리 활성화 여부 |
| **MODE** | Dry-Run (미리보기) 또는 Live (실제 삭제) |
| **GRACE PERIOD** | 삭제 가능 전 대기 시간 |
| **ORPHANED** | 감지된 고아 디렉토리 수 |

### 고아 디렉토리 테이블

| 컬럼 | 설명 |
|------|------|
| 체크박스 | 일괄 삭제를 위한 선택 (Live 모드만) |
| Directory | 확장 가능한 디렉토리명 |
| Path | 전체 파일시스템 경로 |
| Size | 디렉토리 크기 |
| First Seen | 고아 최초 감지 시점 |
| Age | 감지 후 경과 시간 |
| Status | "Can Delete" (유예기간 지남) 또는 "In Grace Period" (유예기간 중) |

### 정리 워크플로우

1. 에이전트가 대응하는 PV가 없는 디렉토리를 감지
2. 디렉토리가 유예기간에 진입 (설정 가능, 기본값: 24시간)
3. 유예기간 이후 상태가 "Can Delete"로 변경
4. **Live 모드**: UI에서 선택하여 즉시 삭제 가능
5. **Dry-Run 모드**: 미리보기만, 실제 삭제 없음

---

## 3. Trends 탭

![Trends 탭](screenshots/03-trends.png)

**Trends** 탭은 용량 계획을 위한 시간별 스토리지 사용 추이를 추적합니다.

### 요약 카드

| 카드 | 설명 | 예시 |
|------|------|------|
| **HISTORY ENTRIES** | 기록된 총 스냅샷 수 | 100 |
| **TRACKED PATHS** | 모니터링 중인 디렉토리 수 | 6 |
| **RETENTION** | 히스토리 보관 기간 | 720h (30일) |

### 사용량 추이 테이블

| 컬럼 | 설명 |
|------|------|
| Directory | 디렉토리명 |
| Current | 현재 사용량 |
| Quota | 설정된 한도 |
| 24H Change | 최근 24시간 사용량 변화 |
| 7D Change | 최근 7일 사용량 변화 |
| 30D Change | 최근 30일 사용량 변화 |
| Trend | 방향 화살표: ↑ (증가), → (안정), ↓ (감소) |

### 추이 분석

- **logs-storage**: 모든 기간에서 +180 MiB, ↑ 추세 — 빠르게 증가 중
- **db-backup**: +50 MiB, ↑ 추세 — 꾸준한 증가
- **app-data**: +45 MiB, ↑ 추세 — 완만한 증가
- **old-app-data / temp-upload**: 쿼터 없음, → 안정
- **staging**: 빈 고아 디렉토리, → 안정

---

## 4. Policies 탭

![Policies 탭](screenshots/04-policies.png)

**Policies** 탭은 네임스페이스 수준의 스토리지 정책과 위반 사항을 표시합니다.

### 네임스페이스 정책

정책은 세 가지 소스에서 파생됩니다 (우선순위 순):
1. **LimitRange** — PersistentVolumeClaim용 Kubernetes LimitRange
2. **Annotation** — 네임스페이스 어노테이션 (`nfs.io/default-quota`, `nfs.io/max-quota`)
3. **Global** — 에이전트의 `--default-quota` 플래그

| 컬럼 | 설명 |
|------|------|
| Namespace | Kubernetes 네임스페이스 |
| Source | 정책 소스 (LimitRange 뱃지) |
| Min | 최소 허용 PVC 크기 |
| Default | 미지정 시 기본 PVC 크기 |
| Max | 최대 허용 PVC 크기 |
| ResourceQuota | 네임스페이스 스토리지 쿼터 사용량 (예: 300Mi / 1Gi = 29%) |

### 테스트 정책 예시

- **default** 네임스페이스: LimitRange 50Mi-500Mi, 기본값 100Mi, ResourceQuota 300Mi/1Gi (29% 사용)
- **test-ns** 네임스페이스: LimitRange 10Mi-200Mi, 기본값 50Mi, ResourceQuota 없음

### 정책 위반

PVC 요청이 네임스페이스의 최대 쿼터를 초과하거나 최소값보다 작을 경우, 하단 테이블에 위반 사항이 표시됩니다:
- 네임스페이스, PVC명, PV명
- 요청 크기 대 최대 허용량
- 위반 유형 (exceeds_max / below_min)

### 정책 설정 방법

```yaml
# PVC 스토리지 제한을 위한 LimitRange
apiVersion: v1
kind: LimitRange
metadata:
  name: storage-limits
  namespace: default
spec:
  limits:
  - type: PersistentVolumeClaim
    max:
      storage: 500Mi
    min:
      storage: 50Mi
    default:
      storage: 100Mi
---
# 네임스페이스 총 스토리지 제한을 위한 ResourceQuota
apiVersion: v1
kind: ResourceQuota
metadata:
  name: storage-quota
  namespace: default
spec:
  hard:
    requests.storage: 1Gi
    persistentvolumeclaims: "10"
```

> **참고**: Helm 차트 배포 시 RBAC에 `limitranges`, `resourcequotas`, `namespaces` 리소스에 대한 읽기 권한이 필요합니다. 최신 차트에는 이미 포함되어 있으므로 `helm upgrade`로 업데이트하세요.

---

## 5. Audit Logs 탭

![Audit Logs 탭](screenshots/05-audit-logs.png)

**Audit Logs** 탭은 모든 쿼터 작업의 완전한 이력을 기록합니다.

### 필터

| 필터 | 옵션 |
|------|------|
| **Action** | All Actions, CREATE, UPDATE, DELETE, CLEANUP |
| **Limit** | 50, 100, 500, 1000 건 |
| **Fails only** | 실패한 작업만 표시 |

### 로그 항목 컬럼

| 컬럼 | 설명 |
|------|------|
| Timestamp | 작업 시간 |
| Action | CREATE / UPDATE / DELETE / CLEANUP |
| PV Name | 연관된 PersistentVolume |
| Namespace | Kubernetes 네임스페이스 |
| Path | NFS 디렉토리 경로 |
| Quota | 적용된 쿼터 크기 |
| Status | 성공 (녹색 체크) 또는 실패 (빨간 X + 에러 메시지) |

### 감사 추적의 이점

- 모든 쿼터 CREATE/UPDATE/DELETE 이벤트 추적
- 실패한 작업 식별 및 문제 해결
- 컴플라이언스 및 변경 관리 문서화
- 작업 유형별 필터링으로 특정 작업 집중 분석

---

## 키보드 단축키

| 키 | 동작 |
|----|------|
| `R` | 데이터 새로고침 |
| `1`-`5` | 탭 전환 |
| `/` | 검색창 포커스 |

---

## 다크 모드

오른쪽 상단의 달 아이콘을 클릭하면 다크 모드로 전환됩니다. 테마 설정은 localStorage에 저장됩니다.

---

## API 참조

대시보드의 모든 데이터는 REST API로 접근 가능합니다:

```bash
# 상태 요약
curl http://localhost:8080/api/status

# PV/PVC 정보를 포함한 쿼터 목록
curl http://localhost:8080/api/quotas

# 감사 로그 (필터 적용)
curl "http://localhost:8080/api/audit?limit=100&action=CREATE"

# 고아 디렉토리
curl http://localhost:8080/api/orphans

# 사용량 추이
curl http://localhost:8080/api/trends

# 네임스페이스 정책
curl http://localhost:8080/api/policies

# 정책 위반
curl http://localhost:8080/api/violations

# 디렉토리 파일 브라우저
curl "http://localhost:8080/api/files?path=/export/default/app-data"
```
