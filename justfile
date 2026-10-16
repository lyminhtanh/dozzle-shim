# dozzle-shim — task runner (just). Chạy `just` hoặc `just --list` để xem tất cả.
#
# Recipe gom theo [group]: build (biên dịch/chạy), release (tag → GitHub Release
# + GHCR image qua CI), ci (theo dõi/kiểm tra workflow & artifact). Pattern học
# từ core-builder justfile (header + [group] + comment Việt/Anh + param mặc định).
#
# Ví dụ:
#   just run tmp/logs                 (build) chạy shim với docker auto-on
#   just release-bump minor           (release) bump tag & push → CI publish
#   just ci-watch                     (ci) xem lần chạy release mới nhất
#   just --list                       (xem tất cả, gom theo nhóm)

set dotenv-load := true

# Biến dùng chung.
repo := "lyminhtanh/dozzle-shim"
listen := ":8080"

# Mặc định: liệt kê các lệnh có sẵn.
default:
    @just --list

# ─────────────────────────────── build ───────────────────────────────

# Chạy shim từ source: tail <logs>, Docker auto-on nếu có socket, UI nhúng sẵn.
[group('build')]
run logs='./logs' glob='*.log':
    go run . -logs {{logs}} -glob '{{glob}}' -listen {{listen}} \
        {{ if path_exists("/var/run/docker.sock") == "true" { "-docker unix:///var/run/docker.sock" } else { "" } }}

# Chạy shim kèm một process qua -exec (stdout+stderr vào UI). vd: just run-exec "java -jar app.jar" api
[group('build')]
run-exec cmd name='app':
    go run . -logs ./logs -glob '*.log' -listen {{listen}} -exec "{{cmd}}" -name {{name}}

# Biên dịch binary self-contained cho mọi target (hoặc 1 target) vào dist/.
[group('build')]
build target='':
    ./build.sh {{target}}

# gofmt + vet + build + test — cổng kiểm tra trước khi release.
[group('build')]
check:
    gofmt -l .
    go vet ./...
    go build -o /dev/null .
    go test ./...

# Build image Docker cục bộ (UI nhúng). tag mặc định: local.
[group('build')]
docker-build tag='local':
    docker build --build-arg VERSION={{tag}} -t dozzle-shim:{{tag}} .

# ────────────────────────────── release ──────────────────────────────

# In version hiện tại (tag gần nhất) và các commit chưa release.
[group('release')]
version:
    @echo "latest tag: $(git describe --tags --abbrev=0 2>/dev/null || echo '(none)')"
    @echo "unreleased commits:"
    @git log --oneline "$(git describe --tags --abbrev=0 2>/dev/null)..HEAD" 2>/dev/null || git log --oneline -5

# Tag & push một version (vd: just release-tag v0.5.0) → CI publish. Chặn nếu cây bẩn / tag đã có.
[group('release')]
release-tag version:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ -n "$(git status --porcelain)" ]; then
        echo "✘ working tree bẩn — commit/push trước khi tag." >&2; exit 1
    fi
    if git rev-parse "{{version}}" >/dev/null 2>&1; then
        echo "✘ tag {{version}} đã tồn tại." >&2; exit 1
    fi
    echo "→ đẩy main trước khi tag…"
    git push origin main
    git tag -a "{{version}}" -m "{{version}}"
    git push origin "{{version}}"
    echo "✓ đã push {{version}} — CI đang publish (xem: just ci-watch)."

# Bump SemVer từ tag gần nhất rồi tag & push. part: major|minor|patch (mặc định patch).
[group('release')]
release-bump part='patch':
    #!/usr/bin/env bash
    set -euo pipefail
    cur="$(git describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)"
    ver="${cur#v}"
    IFS=. read -r MA MI PA <<< "$ver"
    case "{{part}}" in
        major) MA=$((MA+1)); MI=0; PA=0 ;;
        minor) MI=$((MI+1)); PA=0 ;;
        patch) PA=$((PA+1)) ;;
        *) echo "✘ part phải là major|minor|patch" >&2; exit 1 ;;
    esac
    next="v${MA}.${MI}.${PA}"
    echo "bump {{part}}: ${cur} → ${next}"
    just release-tag "${next}"

# ──────────────────────────────── ci ─────────────────────────────────

# Liệt kê các lần chạy workflow release gần đây.
[group('ci')]
ci-list:
    gh run list --workflow=release.yml --limit 5

# Theo dõi lần chạy release mới nhất tới khi xong; in kết luận từng job.
[group('ci')]
ci-watch:
    #!/usr/bin/env bash
    set -euo pipefail
    id="$(gh run list --workflow=release.yml --limit 1 --json databaseId --jq '.[0].databaseId')"
    echo "watching run ${id}…"
    gh run watch "${id}" --exit-status --interval 15 || true
    gh run view "${id}" --json jobs --jq '.jobs[] | "\(.name): \(.conclusion)"'

# Xem release & danh sách asset cho một tag (mặc định: tag gần nhất).
[group('ci')]
release-view tag='':
    #!/usr/bin/env bash
    set -euo pipefail
    t="{{tag}}"; [ -z "$t" ] && t="$(git describe --tags --abbrev=0)"
    gh release view "$t"

# Mở trang GitHub Actions của repo trên trình duyệt.
[group('ci')]
ci-open:
    gh browse --repo {{repo}} --branch main -- .github/workflows/release.yml

# Kiểm tra image GHCR cho một tag đã publish (mặc định tag gần nhất).
[group('ci')]
image-check tag='':
    #!/usr/bin/env bash
    set -euo pipefail
    t="{{tag}}"; [ -z "$t" ] && t="$(git describe --tags --abbrev=0 | sed 's/^v//')"
    echo "pulling ghcr.io/{{repo}}:${t} …"
    docker pull "ghcr.io/{{repo}}:${t}"
