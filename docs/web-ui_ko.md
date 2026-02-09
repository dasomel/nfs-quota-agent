# 웹 UI 가이드

NFS Quota Agent는 NFS 쿼터를 모니터링하고 관리하기 위한 웹 기반 대시보드를 제공합니다.

## 웹 UI 활성화

```bash
# CLI
nfs-quota-agent run --enable-ui --ui-addr=:8080

# Helm
helm install nfs-quota-agent ./charts/nfs-quota-agent \
  --set webUI.enabled=true \
  --set webUI.addr=":8080"
```

`http://<node-ip>:8080`으로 접속

---

## 대시보드 개요

![대시보드 개요](screenshots/01-dashboard-quotas.png)

대시보드는 다음 요약 카드와 함께 실시간 NFS 쿼터 상태를 표시합니다:

| 카드 | 설명 |
|------|------|
| **Total Disk** | NFS export 전체 디스크 용량 |
| **Used** | 현재 디스크 사용량 (퍼센트 포함) |
| **Available** | 여유 디스크 공간 |
| **Directories** | 쿼터가 설정된 디렉토리 수 |
| **Warning** | 쿼터의 90-99%를 사용 중인 디렉토리 |
| **Exceeded** | 쿼터를 초과한 디렉토리 |

---

## 탭

### Quotas 탭

쿼터가 설정된 모든 디렉토리를 보여주는 메인 모니터링 뷰입니다.

**기능:**
- **정렬 가능한 컬럼**: 헤더 클릭으로 정렬
- **검색**: 디렉토리명으로 필터링
- **확장 가능한 행**: 행 클릭 시 디렉토리 내용 조회
- **사용량 바**: 쿼터 사용량의 시각적 표현
- **상태 뱃지**: OK (녹색), Warning (노란색), Exceeded (빨간색)

**컬럼:**
| 컬럼 | 설명 |
|------|------|
| Directory | 디렉토리명 (클릭 시 파일 목록 확장) |
| PV | PersistentVolume 이름 및 바인딩 상태 |
| PVC | PersistentVolumeClaim 이름 및 네임스페이스 |
| Used | 현재 스토리지 사용량 |
| Quota | 설정된 쿼터 한도 |
| Usage | 퍼센트 바 및 수치 |
| Status | OK / Warning / Exceeded / No Quota |

#### 파일 브라우저

행을 클릭하면 디렉토리 내용을 확장하여 볼 수 있습니다:
- 📁 디렉토리 우선 표시
- 📄 파일 크기 정보 포함
- 알파벳순 정렬

---

### Audit 탭

![Audit 탭](screenshots/05-audit-logs.png)

쿼터 작업 이력 조회 (`--enable-audit` 필요).

**필터:**
- **Action**: CREATE, UPDATE, DELETE, CLEANUP
- **Limit**: 항목 수 (50, 100, 500, 1000)
- **Fails only**: 실패한 작업만 표시

**컬럼:**
| 컬럼 | 설명 |
|------|------|
| Timestamp | 작업 시간 |
| Action | CREATE / UPDATE / DELETE / CLEANUP |
| PV Name | 연관된 PersistentVolume |
| Namespace | Kubernetes 네임스페이스 |
| Path | 디렉토리 경로 |
| Quota | 적용된 쿼터 크기 |
| Status | 성공 (✓) 또는 실패 (✗) + 에러 메시지 |

---

### Orphans 탭

![Orphans 탭](screenshots/02-orphans.png)

고아 디렉토리 관리 (`--enable-auto-cleanup` 필요).

**정보 카드:**
- 정리 상태 (Enabled/Disabled)
- 모드 (Dry-Run/Live)
- 유예 기간
- 고아 디렉토리 수

**기능:**
- **체크박스 선택**: 개별 고아 선택
- **전체 선택**: 헤더 체크박스로 일괄 선택
- **Delete Selected**: 선택한 고아 즉시 삭제 (Live 모드만)
- **확장 가능한 행**: 고아 디렉토리 내용 조회

**컬럼:**
| 컬럼 | 설명 |
|------|------|
| ☐ | 선택 체크박스 (Live 모드만) |
| Name | 디렉토리명 |
| Path | 전체 경로 |
| Size | 디렉토리 크기 |
| First Seen | 고아 최초 감지 시점 |
| Age | 감지 후 경과 시간 |
| Status | Can Delete / In Grace Period |

#### 고아 삭제

**Live 모드** (cleanup.dryRun=false)에서:
1. 체크박스로 고아 선택
2. "Delete Selected" 버튼 클릭
3. 확인 대화상자에서 삭제 확인
4. 고아 즉시 제거

---

### Trends 탭

![Trends 탭](screenshots/03-trends.png)

사용량 히스토리 및 추이 조회 (`--enable-history` 필요).

**정보 카드:**
- 히스토리 항목 수
- 추적 중인 경로 수
- 보관 기간

**컬럼:**
| 컬럼 | 설명 |
|------|------|
| Directory | 디렉토리명 |
| Current | 현재 사용량 |
| Quota | 쿼터 한도 |
| 24h Change | 최근 24시간 사용량 변화 |
| 7d Change | 최근 7일 사용량 변화 |
| 30d Change | 최근 30일 사용량 변화 |
| Trend | ↑ (증가) / ↓ (감소) / → (안정) |

---

### Policies 탭

![Policies 탭](screenshots/04-policies.png)

네임스페이스 쿼터 정책 조회 (`--enable-policy` 필요).

**표시 내용:**
- 네임스페이스 수준 쿼터 정책
- LimitRange 설정
- ResourceQuota 사용량
- 정책 위반 사항

**우선순위:**
1. LimitRange (PersistentVolumeClaim 제한)
2. 네임스페이스 어노테이션 (`nfs.io/default-quota`, `nfs.io/max-quota`)
3. 글로벌 기본값 (`--default-quota`)

---

## API 엔드포인트

웹 UI는 다음 REST API를 사용합니다:

| 엔드포인트 | 메서드 | 설명 |
|------------|--------|------|
| `/api/status` | GET | 디스크 및 쿼터 요약 |
| `/api/quotas` | GET | 전체 쿼터 목록 |
| `/api/config` | GET | 기능 플래그 |
| `/api/audit` | GET | 감사 로그 항목 |
| `/api/orphans` | GET | 고아 디렉토리 |
| `/api/orphans/delete` | POST | 고아 삭제 |
| `/api/files` | GET | 디렉토리 내용 |
| `/api/history` | GET | 사용량 히스토리 |
| `/api/trends` | GET | 사용량 추이 |
| `/api/policies` | GET | 네임스페이스 정책 |
| `/api/violations` | GET | 정책 위반 |

### API 호출 예시

```bash
# 쿼터 상태 조회
curl http://localhost:8080/api/status

# 쿼터 목록 조회
curl http://localhost:8080/api/quotas

# 디렉토리 내용 조회
curl "http://localhost:8080/api/files?path=/export/default"

# 고아 삭제 (Live 모드만)
curl -X POST http://localhost:8080/api/orphans/delete \
  -H "Content-Type: application/json" \
  -d '{"path":"/export/orphan-dir"}'
```

---

## 키보드 단축키

| 키 | 동작 |
|----|------|
| `R` | 데이터 새로고침 |
| `1-5` | 탭 전환 |
| `/` | 검색창 포커스 |

---

## 문제 해결

### 탭이 보이지 않는 경우

탭은 활성화된 기능에 따라 표시됩니다:

| 탭 | 필요한 플래그 |
|----|---------------|
| Audit | `--enable-audit` |
| Orphans | `--enable-auto-cleanup` |
| Trends | `--enable-history` |
| Policies | `--enable-policy` |

### 쿼터 목록이 비어 있는 경우

1. NFS 경로가 올바르게 마운트되었는지 확인
2. 파일시스템에 프로젝트 쿼터가 활성화되었는지 확인
3. 에이전트 로그 확인: `kubectl logs -n nfs-quota-agent deploy/nfs-quota-agent`

### 삭제 버튼이 보이지 않는 경우

고아 삭제에 필요한 조건:
- `--enable-auto-cleanup`
- `--cleanup-dry-run=false` (Live 모드)
