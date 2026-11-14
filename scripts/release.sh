#!/usr/bin/env bash
# 发布新版本：scripts/release.sh 0.2.0
#   1. 检查工作区干净、在 main 上、测试通过
#   2. 打 tag vX.Y.Z 并推送，创建 GitHub Release
#   3. 计算源码包 sha256，更新 nickchef/homebrew-tap 里的 Formula/ssync.rb 并推送
set -euo pipefail

REPO="nickchef/ssync"
TAP="nickchef/homebrew-tap"

ver="${1:-}"
ver="${ver#v}"
if [[ ! "$ver" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "用法：$0 <版本号，例如 0.2.0>" >&2
  exit 2
fi
tag="v$ver"

cd "$(dirname "$0")/.."
[[ "$(git rev-parse --abbrev-ref HEAD)" == "main" ]] || { echo "请在 main 分支上发布" >&2; exit 1; }
[[ -z "$(git status --porcelain)" ]] || { echo "工作区有未提交的改动" >&2; exit 1; }
git rev-parse -q --verify "refs/tags/$tag" >/dev/null && { echo "tag $tag 已存在" >&2; exit 1; }

go vet ./...
go test ./...

git push origin main
git tag -a "$tag" -m "ssync $tag"
git push origin "$tag"
gh release create "$tag" --repo "$REPO" --title "ssync $tag" --generate-notes

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

url="https://github.com/$REPO/archive/refs/tags/$tag.tar.gz"
curl -fsSL --retry 5 --retry-delay 2 -o "$work/src.tar.gz" "$url"
sha="$(shasum -a 256 "$work/src.tar.gz" | awk '{print $1}')"

gh repo clone "$TAP" "$work/tap" -- --quiet
mkdir -p "$work/tap/Formula"
sed -e "s|@URL@|$url|" -e "s|@SHA256@|$sha|" packaging/ssync.rb.tmpl > "$work/tap/Formula/ssync.rb"

cd "$work/tap"
git add Formula/ssync.rb
git commit -q -m "ssync $tag"
git push -q origin HEAD

echo "已发布 $tag"
echo "安装：brew install nickchef/tap/ssync    升级：brew update && brew upgrade ssync"
