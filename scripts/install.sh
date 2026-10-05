#!/bin/sh
# cpagw 安装脚本：从 GitHub Release 下载对应平台产物，校验后安装。
#
#   curl -fsSL https://raw.githubusercontent.com/shichao-wang/cpagw/main/scripts/install.sh | bash
#
# 环境变量：
#   CPAGW_VERSION        指定版本（如 v0.1.0），默认取最新 Release
#   CPAGW_INSTALL_DIR    指定安装目录，默认 /usr/local/bin，不可写时回退 ~/.local/bin
set -eu

REPO="shichao-wang/cpagw"
BIN_NAME="cpagw"
ASSET_NAME="cpagw"

err() {
	printf '错误：%s\n' "$1" >&2
	exit 1
}

info() {
	printf '%s\n' "$1"
}

need() {
	command -v "$1" >/dev/null 2>&1 || err "缺少命令 $1，请先安装"
}

need uname
need tar

# 产物导出函数名只依赖 uname，不依赖 env/awk 等可能在受限环境缺失的命令。
detect_os() {
	case "$(uname -s)" in
	Darwin) printf 'darwin' ;;
	Linux) printf 'linux' ;;
	*) err "不支持的操作系统：$(uname -s)，本工具当前仅支持 macOS 与 Linux" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) printf 'amd64' ;;
	arm64 | aarch64) printf 'arm64' ;;
	*) err "不支持的架构：$(uname -m)" ;;
	esac
}

download() {
	url="$1"
	dest="$2"
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$url" -o "$dest"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$dest" "$url"
	else
		err "需要 curl 或 wget 下载安装包"
	fi
}

fetch() {
	# 静默输出到 stdout；失败时返回非零，由调用方决定是否致命。
	url="$1"
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$url" 2>/dev/null
	elif command -v wget >/dev/null 2>&1; then
		wget -qO- "$url" 2>/dev/null
	else
		err "需要 curl 或 wget 下载安装包"
	fi
}

sha256_of() {
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d ' ' -f 1
	elif command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d ' ' -f 1
	else
		printf ''
	fi
}

resolve_version() {
	if [ -n "${CPAGW_VERSION:-}" ]; then
		printf '%s' "$CPAGW_VERSION"
		return
	fi
	# 从 releases/latest 的 JSON 中取 tag_name；也可改用 /releases/latest 的跳转地址。
	tag="$(fetch "https://api.github.com/repos/${REPO}/releases/latest" |
		grep '"tag_name"' | head -n1 | cut -d '"' -f 4)"
	[ -n "$tag" ] || err "无法获取最新版本，请用 CPAGW_VERSION 指定版本"
	printf '%s' "$tag"
}

OS="$(detect_os)"
ARCH="$(detect_arch)"
VERSION="$(resolve_version)"
ARCHIVE="${ASSET_NAME}_${OS}_${ARCH}.tar.gz"
BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/cpagw-install.XXXXXX")"

cleanup() {
	rm -rf "$TMP_DIR"
}
trap cleanup EXIT INT TERM

info "平台：${OS}/${ARCH}"
info "版本：${VERSION}"
info "下载：${BASE_URL}/${ARCHIVE}"

download "${BASE_URL}/${ARCHIVE}" "${TMP_DIR}/${ARCHIVE}"

# 校验和是强制环节，不提供跳过开关：能控制 Release 的攻击者可以同时替换产物与
# checksums.txt，因此它防的是传输损坏与只改动产物一侧的篡改，属于完整性检查。
download "${BASE_URL}/checksums.txt" "${TMP_DIR}/checksums.txt"
expected="$(grep " ${ARCHIVE}\$" "${TMP_DIR}/checksums.txt" | cut -d ' ' -f 1 | head -n1)"
[ -n "$expected" ] || err "checksums.txt 中缺少 ${ARCHIVE} 的记录"
actual="$(sha256_of "${TMP_DIR}/${ARCHIVE}")"
[ -n "$actual" ] || err "找不到 shasum 或 sha256sum，无法校验安装包完整性；请先安装 coreutils 后重试"
[ "$expected" = "$actual" ] || err "校验和不匹配，已中止安装"
info "校验和：ok"

tar -xzf "${TMP_DIR}/${ARCHIVE}" -C "$TMP_DIR"
[ -f "${TMP_DIR}/${BIN_NAME}" ] || err "压缩包中未找到 ${BIN_NAME}"
chmod +x "${TMP_DIR}/${BIN_NAME}"

install_dir() {
	if [ -n "${CPAGW_INSTALL_DIR:-}" ]; then
		printf '%s' "$CPAGW_INSTALL_DIR"
		return
	fi
	if [ -w /usr/local/bin ]; then
		printf '/usr/local/bin'
		return
	fi
	# /usr/local/bin 不可写时回退到用户目录，避免强制使用 sudo。
	printf '%s' "${HOME}/.local/bin"
}

INSTALL_DIR="$(install_dir)"
mkdir -p "$INSTALL_DIR" || err "无法创建目录 ${INSTALL_DIR}"
[ -w "$INSTALL_DIR" ] || err "目录不可写：${INSTALL_DIR}"

# 先安装到临时名再原子改名，避免覆盖正在运行的 binary 时报错。
mv "${TMP_DIR}/${BIN_NAME}" "${INSTALL_DIR}/.${BIN_NAME}.tmp.$$"
chmod +x "${INSTALL_DIR}/.${BIN_NAME}.tmp.$$"
mv "${INSTALL_DIR}/.${BIN_NAME}.tmp.$$" "${INSTALL_DIR}/${BIN_NAME}"

info "已安装：${INSTALL_DIR}/${BIN_NAME}"
"${INSTALL_DIR}/${BIN_NAME}" --help >/dev/null 2>&1 || info "提示：${INSTALL_DIR}/${BIN_NAME} 暂不能正常执行，请检查平台与架构是否匹配"

case ":${PATH}:" in
*":${INSTALL_DIR}:"*) ;;
*)
	info ""
	info "${INSTALL_DIR} 不在 PATH 中，请加入 shell 配置："
	info "  export PATH=\"${INSTALL_DIR}:\$PATH\""
	;;
esac
