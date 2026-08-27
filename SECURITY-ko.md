# 보안 정책 (Security Policy)

[English](SECURITY.md) | 한국어

## 지원 대상 버전

| 버전 | 지원 여부 |
| ---- | -------- |
| v0.x | :white_check_mark: |

## 보안 모델 및 권한 (Security Model)

`nfs-quota-agent`는 마운트된 NFS 볼륨의 사용량을 측정(`quotactl`, `xfs_quota` 등)하고 쿼터를 적용하기 위해 노드 데몬 권한으로 실행됩니다.

- 에이전트 엔드포인트는 mTLS 또는 Kubernetes RBAC 인증 하에 보호되어야 합니다.
- 쿼터 조정 요청은 경로 탐색(path traversal) 공격을 방지하기 위해 볼륨 경계를 철저히 검증해야 합니다.
- 일반 텍스트 인증 정보나 볼륨 시크릿을 로그에 절대 노출하지 않습니다.

## 취약점 보고 절차 (Reporting a Vulnerability)

보안 취약점은 공개 GitHub Issue로 등록하지 마시고, GitHub Private Vulnerability Reporting 또는 관리자 이메일을 통해 비공개로 보고해 주십시오. 48시간 이내에 접수 확인 및 조치 계획을 안내합니다.

참조: [OpenForge Security Standard](https://github.com/dasomel/openforge/blob/main/docs/security.md)
