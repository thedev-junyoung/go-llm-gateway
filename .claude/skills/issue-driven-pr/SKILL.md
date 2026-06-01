---
name: issue-driven-pr
description: 이 repo 에서 코드를 만지기 전에 사용. 모든 변경이 issue → branch → PR → AI 리뷰(pr-reviewer + adr-critic) + CI → squash-merge to main 흐름을 따르도록 강제한다. main 에 직접 commit 은 1회 부트스트랩 외에는 금지.
---

# 이슈 기반 PR 워크플로우 (`thedev-junyoung-go-llm-gateway`)

> **규칙:** issue, PR, 자체 AI 리뷰어 판정, green CI 없이는 어떤 변경도 `main` 에 머지되지 않는다. 부트스트랩은 유일한 예외 (commit history 에 기록).

이 skill 은 **rigid** — 체크리스트를 그대로 따른다. 핵심은 규율 그 자체다. 모든 변경이 ADR 형태의 paper trail 이 된다 (issue = 문제, PR = 해결안, 리뷰 = 비평).

## 언제 호출

다음 작업을 사용자가 요청할 때마다 호출:
- feature, fix, refactor 구현
- docs, ADR, CI 수정
- 테스트, examples, tooling 추가
- 머지 가능한 diff 를 만드는 모든 변경

`main` 에 `git commit` 하려는 순간이 오면 **멈추고** step 1 로 돌아간다.

## 7-step 체크리스트

각 step 마다 TodoWrite 항목을 만들고 진행하며 체크.

### 1. 사용자와 스코프 확정
- 한 문장: 무엇을 바꾸고, *왜* 인가?
- 변경이 모듈 경계(`docs/design/v0.1-scope.md`) 를 건드리거나 비자명한 결정을 함의하면 surface — 코드 전에 ADR 가 필요할 수 있다.

### 2. GitHub issue 생성
- `gh issue create --title "<type>: <short imperative>" --body "..."`
- Issue body 는 **평범한 산문**. 문제 → 제안 → 범위 외 순서면 충분. 체크박스/이모지/표 남발 금지 — 진짜 체크할 항목이나 비교가 있을 때만.
- "draft, pending review" 같은 자기 narration 류 disclaimer 는 issue body 에 넣지 않는다 (그건 PR body 에서 필요하면 한 줄).
- Title prefix 는 Conventional Commits 준수: `feat:`, `fix:`, `chore:`, `docs:`, `refactor:`, `test:`, `perf:`, `ci:`, `build:`.
- Issue 번호 캡쳐 — 이후 모든 산출물이 이걸 참조한다.

### 3. clean main 에서 branch
```bash
git switch main
git pull --ff-only
git switch -c <type>/<issue-num>-<slug>     # feat/12-provider-interface
```
- Branch 이름: `<type>/<issue-num>-<kebab-slug>`. slug 는 4단어 이하.
- 사용자가 명시 지시 없이는 다른 feature branch 에서 분기하지 않는다.

### 4. branch 에서 commit
- **Conventional Commits** title: `<type>(<scope>)?: <imperative>`. scope 는 선택, lowercase.
- 명시적 경로로 stage (`git add path/to/file`). `git add .` / `git add -A` 금지.
- `--no-verify` 금지. hook 실패 시 root cause 고치고 **새 commit** (`--amend` 금지).
- Commit 은 atomic — 논리적 변경 하나당 commit 하나. 멀티 스텝 작업 = 멀티 commit PR.
- Body 는 *무엇이 바뀌었고 왜* 만. "claude[bot] caught X", "agent draft" 류 자기 narration 류 표현 안 씀.
- 모든 commit 끝에 `Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>` 트레일러. 이게 본 repo 의 honesty signal — agent-assisted 임을 git history 에 남기는 단 한 줄.

### 5. Push + PR 생성
```bash
git push -u origin <branch>
gh pr create --base main --title "<issue 와 동일하거나 정제된 표현>" --body "..."
```
- PR body 첫 줄은 `Closes #<issue-num>` — 머지 시 issue 자동 close.
- 본문은 **평범한 산문**. 무엇이 바뀌었고 리뷰어가 어디를 봐야 하는지가 핵심. "Summary / Test plan / Out of scope" 같은 rigid template 강제 안 함 — 진짜 actionable checkbox 가 있을 때만 checkbox 사용. 이모지 (✅, 🎉, 🤖) 메인 메시지에서 빼기.
- "draft, pending review", "agent draft" 류 self-narration 안 씀. 필요하면 한 줄로 "interface decisions land via ADR-N before code" 같이 사실로 진술.
- **유지보수자의 비공개 머지 게이트** (`docs/workflow/agent-driven-development.md`) 를 PR body / commit 메시지에 옮겨 적지 말 것 — 사고 도구지 템플릿 항목 아님.

### 6. AI 리뷰 + CI 대기
- **AI 리뷰어** 가 `.github/workflows/ai-review.yml` 를 통해 자동 실행:
  - `code-reviewer` 잡 — `pr-reviewer` skill (`.claude/skills/pr-reviewer/SKILL.md`) 으로 broad code review
  - `adr-critic` 잡 — `docs/adr/**` 변경 시에만; `adr-critic` skill 사용
- **CI** (`.github/workflows/ci.yml`) 가 green 이어야 한다: `go build`, `go test -race`, `golangci-lint`.
- 리뷰 판정과 CI **둘 다** clean 되기 전엔 머지하지 않는다. 리뷰어가 동의 안 가는 지적을 하면 PR 에 reasoning 으로 답변 — 침묵으로 무시하지 말 것.
- 피드백이 불명확하거나 기술적으로 의심스러우면 `superpowers:receiving-code-review` skill 사용.

### 7. main 으로 squash-merge
```bash
gh pr merge <pr-num> --squash --delete-branch
```
- **Squash** 가 기본 — `main` history 를 linear 하게, issue 당 commit 1개로 유지.
- Squash commit 메시지: PR title + body 그대로 (PR body 가 squash commit body 가 된다).
- 머지 후 branch 삭제.
- 즉시 `main` pull: `git switch main && git pull --ff-only`.

## 하드룰 (예외 없음)

| ❌ Never | ✅ Always |
|---|---|
| `main` 으로 직접 push | feature branch 에 push, main 으로 PR |
| AI 리뷰 판정 없이 머지 | `code-reviewer` (해당하면 `adr-critic` 도) 코멘트 대기 |
| CI 빨강에서 머지 | CI fix, fixup push, 재실행 |
| `git add .` / `git add -A` | `git add <explicit-path>` |
| hook 회피 (`--no-verify`) | lint/format/test 실패 root cause 수정 |
| `main` 으로 `--force-push` | force-push 는 본인 feature branch 에서 `--force-with-lease` 만 |
| push 된 commit `--amend` | 새 commit; squash on merge 가 cleanup |
| 리뷰어 피드백 침묵 무시 | reasoning 으로 답변 후 resolve |

## 부트스트랩 예외 (1회 한정)

repo init, `go mod init`, CI 골격, LICENSE, 초기 docs, agent-review setup 자체 같은 최초 스캐폴딩 commit 들은 이 skill 강제 시점 *이전이라* 직접 `main` 에 머지될 수 있다. 그 이후로는 **모든** 변경이 예외 없이 이 skill 을 따른다.

## AI 리뷰 setup (1회, 사람 손)

자체 "에이전트 팀" 이 외부 유료 리뷰어를 대체한다. 프로젝트 메모리 `project_ai_pr_review.md` 도 참고.

두 인증 경로 중 **하나** 선택. GitHub App 경로 권장 — OIDC 기반, rotate 할 secret 없음.

### Option A — Claude Code GitHub App (권장)

1. Claude Code interactive 세션에서:
   ```
   /install-github-app
   ```
   대상 repo 에 **Claude Code** GitHub App 설치 + OIDC 인증 자동 wiring. `secrets.*` 관리 불필요.
2. 수동 fallback: https://github.com/apps/claude 에서 `thedev-junyoung/thedev-junyoung-go-llm-gateway` 에 설치.

### Option B — OAuth token secret

App 경로 불가 (예: org 정책으로 App 설치 금지) 시에만.

1. 로컬: `claude setup-token` → token 복사.
2. Repo Settings → Secrets and variables → Actions → New repository secret
   - Name: `CLAUDE_CODE_OAUTH_TOKEN`
   - Value: 1번 토큰.

### Branch protection (어느 인증 경로든 공통)

Settings → Branches → Add rule for `main`:
- Require a pull request before merging
- Require status checks: `test`, `lint`, `code-reviewer`
- Require conversation resolution before merging

### 리뷰어 튜닝

skill 파일 (`.claude/skills/pr-reviewer/SKILL.md`, `.claude/skills/adr-critic/SKILL.md`) 을 PR 로 수정. skill 파일이 진실의 원천 — 리뷰 지시를 `ai-review.yml` 안에 inline 으로 쓰지 **말 것**.

## 안티패턴 (발견하면 즉시 멈춤)

- "딱 이번만 main 에 직접 commit" — no. 부트스트랩 끝났다.
- "리뷰어가 noisy 하니까 그냥 머지" — no. reasoning 답변 후 머지.
- "관련 없는 변경 5개를 한 PR 로 묶어 리뷰 사이클 절약" — no. issue 1개 = 관심사 1개 = PR 1개.
- "ADR 은 나중에" — no. PR 이 비자명한 결정을 담는다면 ADR 도 같은 PR (또는 선행 PR) 에서 같이 land.
- "PR body 에 merge gate 인용해서 진지함 증명" — no. 게이트는 비공개 사고 도구; 리뷰어 보는 산출물에 인용하면 cargo-cult.

## 이 룰이 존재하는 이유

`docs/workflow/agent-driven-development.md`: **타이핑은 위임 OK. 사고는 위임 NOT OK.** Issue 는 사람이 코드 전에 의도를 *명료화* 하도록 강제한다. PR 은 AI 리뷰어 (와 사람) 가 main 직전에 *비판* 하도록 강제한다. 이 gate 없이는 에이전트의 타이핑 속도가 자산이 아니라 부채가 된다.
