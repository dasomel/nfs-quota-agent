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
| **Remaining** | 여유 공간 비율 및 상태별로 색상이 적용되는 핵심 디스크 잔여 용량 지표 카드 |
| **Directories** | 쿼터가 설정된 디렉토리 수 |
| **Warning** | 쿼터의 90-99%를 사용 중인 디렉토리 |
| **Exceeded** | 쿼터를 초과한 디렉토리 |

### 네임스페이스별 요약 (Namespace Summary)
요약 카드 하단에는 **Usage by Namespace** 패널이 있어 Kubernetes 네임스페이스별로 집계된 총 스토리지 사용량을 가로 바 차트로 보여줍니다. 각 네임스페이스별 Limit 대비 사용 현황을 한눈에 파악할 수 있어 멀티테넌트 환경 모니터링이 용이합니다.

### 공통 UI 기능
- **언어 토글 (Language Toggle)**: 헤더 우측의 `🇰🇷 KO` / `🇺🇸 EN` 버튼을 클릭하여 한국어와 영어를 손쉽게 토글할 수 있습니다. 설정 상태는 브라우저의 `localStorage`에 자동 저장됩니다.
- **자동 새로고침 (Auto-Refresh)**: ⏸️/▶️ 버튼과 새로고침 주기 선택박스(5초, 10초, 30초, 60초)를 통해 화면을 자동 갱신할 수 있습니다. 시스템 부하 및 대역폭 절약을 위해 현재 활성화된 탭의 API만 백그라운드로 갱신합니다.

---

## 탭

### Quotas 탭

쿼터가 설정된 모든 디렉토리를 보여주는 메인 모니터링 뷰입니다.

**기능:**
- **정렬 가능한 컬럼**: 헤더 클릭으로 정렬을 수행하며, 갱신 시에도 마지막 정렬 상태가 유지됩니다.
- **검색**: 디렉토리명 및 경로로 필터링할 수 있습니다.
- **확장 가능한 행**: 행 클릭 시 해당 디렉토리의 파일 브라우저가 하단에 열려 파일 및 디렉토리 내부 구성을 바로 확인할 수 있습니다.
- **사용량 바**: 쿼터 사용량의 시각적 표현.
- **CSV 내보내기**: **📥 Export CSV** 버튼을 통해 클라이언트 사이드에서 즉시 생성한 쿼터 현황 CSV 보고서를 다운로드할 수 있습니다.
- **상태 뱃지**: OK (녹색), Warning (노란색), Exceeded (빨간색)
- **디스크 사용량 도넛 차트 (Donut Chart)**: Used vs Remaining 스토리지 링과 상태별 색상 아크를 제공하며, 중앙에 사용률을 표기하고 호버 시 바이트와 인간 친화적 크기 툴팁을 제공합니다.
- **디렉토리 사용량 가로 바 차트 (Horizontal Bar Chart)**: 사용량 상위 8개 디렉토리를 sequentialBlue mid step 색상으로 표시하고 나머지는 '기타'로 묶어 제공합니다. 바를 클릭하면 해당 디렉토리 검색 필터가 적용됩니다.

**컬럼:**
| 컬럼 | 설명 |
|------|------|
| Directory | 디렉토리명 (클릭 시 파일 목록 확장) |
| PV | PersistentVolume 이름 및 바인딩 상태 |
| PVC | PersistentVolumeClaim 이름 및 네임스페이스 |
| Used | 현재 스토리지 사용량 |
| Quota | 설정된 쿼터 한도 |
| Remaining | 쿼터 남은 용량 (Quota - Used) |
| Usage | 퍼센트 바 및 수치 |
| Quota Status | 쿼터 동기화 상태: `Applied` (적용됨 - 녹색), `Pending` (대기 중 - 노란색), `Failed` (실패 - 빨간색) |
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

**기능:**
- **CSV 내보내기**: **📥 Export CSV** 버튼으로 감사 로그 내역을 CSV 파일로 다운로드합니다.
- **필터**: CREATE, UPDATE, DELETE, CLEANUP 액션별 필터링 및 실패한(Fails only) 로그만 모아보는 기능 지원.

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
- **Scan Now (즉시 스캔)**: 즉시 고아 디렉토리를 탐색하는 백엔드 스캔을 실행합니다.
- **Clean Up (일괄 정리)**: 클릭 시 시뮬레이션(Dry-run) 결과를 인라인 승인 패널로 보여준 뒤, 일괄 비대화형 정리 프로세스 실행 동의를 요청합니다.
- **체크박스 선택**: 개별 고아 디렉토리를 선택하여 대상 삭제할 수 있습니다.
- **Delete Selected**: 선택한 고아 디렉토리를 일괄 삭제합니다 (Live 모드에서 확인 패널을 거쳐 즉시 반영).
- **Btrfs 안내**: 파일시스템이 Btrfs 환경일 경우, Btrfs qgroup 관리 특성상 project/projid 기반 고아 정리 대상에서 제외됨을 명시하는 안내문이 노출됩니다.

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
2. "Delete Selected" 또는 "Clean Up" 버튼 클릭
3. 인라인 확인 패널에서 최종 실행 여부 확인
4. 고아 디렉토리 즉시 제거

---

### Trends 탭

![Trends 탭](screenshots/03-trends.png)

사용량 히스토리 및 추이 조회 (`--enable-history` 필요).

**SVG 사용량 차트:**
사용량 상위 4개 디렉토리 경로 및 '기타(Other)' 통합 시리즈의 시간 경과에 따른 사용량 변화 이력을 다이내믹 SVG 라인 차트로 렌더링합니다. 범주형 팔레트 고정 순서, 직접 엔드 라벨링, 그리고 특정 시점의 모든 시리즈 상세 값을 표시하는 크로스헤어 호버 툴팁이 적용되어 있습니다.

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

네임스페이스 쿼터 정책 조회 (`--enable-policy` 필요). **자문(advisory)용입니다** — 이 화면은 정보
제공용이며 실제 쿼터 크기에는 영향을 주지 않습니다. 자세한 내용은
[`feature-guide_ko.md`](feature-guide_ko.md#4-policies-탭) 참고.

**표시 내용:**
- 네임스페이스 수준 쿼터 정책
- LimitRange 설정 (라벨 하단에 LimitRange의 실제 객체명 상세 노출)
- ResourceQuota 사용량 (객체명 및 상세 쿼터 리밋 정보)
- 정책 위반 사항 (Exceeds Max / Below Min 위반 배지)

---

## API 엔드포인트

웹 UI는 다음 REST API를 사용합니다:

| 엔드포인트 | 메서드 | 설명 |
|------------|--------|------|
| `/api/status` | GET | 디스크 및 쿼터 요약 |
| `/api/quotas` | GET | 전체 쿼터 목록 |
| `/api/config` | GET | 기능 플래그 |
| `/api/audit` | GET | 감사 로그 항목 |
| `/api/orphans` | GET | 고아 디렉토리 목록 |
| `/api/orphans/scan` | POST | 고아 스캔 즉시 실행 |
| `/api/orphans/cleanup`| POST | 고아 정리 실행 |
| `/api/orphans/delete` | POST | 개별 고아 삭제 |
| `/api/files` | GET | 디렉토리 내용 |
| `/api/history` | GET | 사용량 히스토리 |
| `/api/trends` | GET | 사용량 추이 |
| `/api/policies` | GET | 네임스페이스 정책 |
| `/api/violations` | GET | 정책 위반 |

---

## 키보드 단축키

| 키 | 동작 |
|----|------|
| `R` | 데이터 새로고침 |
| `1-5` | 탭 전환 |
| `/` | 검색창 포커스 |

*참고: 입력창(input, textarea)이나 선택박스(select) 내부에 포커스가 들어가 있는 상태에서는 키보드 단축키가 자동으로 비활성화됩니다.*

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
