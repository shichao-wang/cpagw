#!/usr/bin/env python3
"""验证 GoReleaser snapshot 包结构、校验和，并运行当前平台 binary smoke test。"""

from __future__ import annotations

import argparse
import hashlib
import os
import platform
import re
import subprocess
import struct
import sys
import tarfile
import tempfile
from pathlib import Path

ARCHIVES = {
    "cpagw_darwin_amd64.tar.gz": ("darwin", "amd64"),
    "cpagw_darwin_arm64.tar.gz": ("darwin", "arm64"),
    "cpagw_linux_amd64.tar.gz": ("linux", "amd64"),
    "cpagw_linux_arm64.tar.gz": ("linux", "arm64"),
}
REQUIRED_FILES = {"cpagw", "README.md", "THIRD_PARTY_NOTICES"}
BUILD_OUTPUTS = {
    "cpagw_darwin_amd64_v1",
    "cpagw_darwin_arm64_v8.0",
    "cpagw_linux_amd64_v1",
    "cpagw_linux_arm64_v8.0",
}
CHECKSUM_LINE = re.compile(r"^([0-9a-fA-F]{64})  ([^/\\\r\n]+)$")


class VerificationError(Exception):
    pass


def native_target() -> tuple[str, str] | None:
    goos = {"Darwin": "darwin", "Linux": "linux"}.get(platform.system())
    goarch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(
        platform.machine()
    )
    if goos is None or goarch is None:
        return None
    return goos, goarch


def verify_checksums(dist: Path) -> None:
    checksum_path = dist / "checksums.txt"
    try:
        lines = checksum_path.read_text(encoding="ascii").splitlines()
    except (OSError, UnicodeError) as error:
        raise VerificationError(f"无法读取 checksums.txt：{error}") from error

    entries: dict[str, str] = {}
    for line in lines:
        match = CHECKSUM_LINE.fullmatch(line)
        if match is None:
            raise VerificationError(f"checksums.txt 格式错误：{line!r}")
        digest, name = match.groups()
        if name in entries:
            raise VerificationError(f"checksums.txt 存在重复条目：{name}")
        entries[name] = digest.lower()

    expected = set(ARCHIVES)
    if set(entries) != expected:
        missing = sorted(expected - set(entries))
        unexpected = sorted(set(entries) - expected)
        raise VerificationError(
            f"checksums.txt 条目不匹配；缺少={missing}，多余={unexpected}"
        )

    for name, expected_digest in entries.items():
        digest = hashlib.sha256((dist / name).read_bytes()).hexdigest()
        if digest != expected_digest:
            raise VerificationError(f"SHA256 不匹配：{name}")


def binary_target(binary: bytes) -> tuple[str, str] | None:
    if binary.startswith(b"\x7fELF") and len(binary) >= 20:
        byte_order = {1: "<", 2: ">"}.get(binary[5])
        if byte_order is None:
            return None
        machine = struct.unpack(f"{byte_order}H", binary[18:20])[0]
        arch = {62: "amd64", 183: "arm64"}.get(machine)
        return ("linux", arch) if arch else None
    if binary.startswith(b"\xcf\xfa\xed\xfe") and len(binary) >= 8:
        cpu_type = struct.unpack("<I", binary[4:8])[0]
        arch = {0x01000007: "amd64", 0x0100000C: "arm64"}.get(cpu_type)
        return ("darwin", arch) if arch else None
    return None


def verify_archive(path: Path) -> None:
    try:
        with tarfile.open(path, mode="r:gz") as archive:
            members = archive.getmembers()
            binary_member = archive.extractfile("cpagw")
            binary_header = binary_member.read(32) if binary_member is not None else b""
    except (OSError, KeyError, tarfile.TarError) as error:
        raise VerificationError(f"无法读取归档 {path.name}：{error}") from error

    expected_target = ARCHIVES.get(path.name)
    if expected_target is None or binary_target(binary_header) != expected_target:
        raise VerificationError(f"归档平台或架构与 binary 不匹配：{path.name}")

    names = [member.name for member in members]
    if len(names) != len(set(names)):
        raise VerificationError(f"归档包含重复路径：{path.name}")
    if set(names) != REQUIRED_FILES:
        raise VerificationError(
            f"归档文件清单不匹配：{path.name}，实际={sorted(names)}"
        )
    for member in members:
        if not member.isfile() or member.size <= 0:
            raise VerificationError(f"归档条目不是非空普通文件：{path.name}:{member.name}")
        if member.name == "cpagw" and member.mode & 0o111 == 0:
            raise VerificationError(f"归档中的 cpagw 不可执行：{path.name}")


def verify_native_binary(dist: Path) -> None:
    target = native_target()
    if target is None:
        raise VerificationError(
            f"不支持在当前平台执行原生 smoke test：{platform.system()}/{platform.machine()}"
        )
    matching = [name for name, artifact_target in ARCHIVES.items() if artifact_target == target]
    if len(matching) != 1:
        raise VerificationError(f"没有唯一的原生归档：{target}")

    archive_path = dist / matching[0]
    with tempfile.TemporaryDirectory(prefix="cpagw-release-smoke-") as temporary:
        temporary_path = Path(temporary)
        binary_path = temporary_path / "cpagw"
        home_path = temporary_path / "home"
        config_path = temporary_path / "config"
        state_path = temporary_path / "state"
        home_path.mkdir()
        config_path.mkdir()
        state_path.mkdir()
        try:
            with tarfile.open(archive_path, mode="r:gz") as archive:
                member = archive.extractfile("cpagw")
                if member is None:
                    raise VerificationError(f"归档缺少 cpagw binary：{archive_path.name}")
                binary_path.write_bytes(member.read())
        except (OSError, tarfile.TarError) as error:
            raise VerificationError(f"无法提取原生 binary：{error}") from error
        binary_path.chmod(0o700)
        environment = os.environ.copy()
        environment.update({"HOME": str(home_path), "XDG_CONFIG_HOME": str(config_path)})
        try:
            result = subprocess.run(
                [str(binary_path), "--state-dir", str(state_path), "version"],
                check=False,
                capture_output=True,
                text=True,
                timeout=20,
                env=environment,
            )
        except subprocess.TimeoutExpired as error:
            raise VerificationError("原生 binary version smoke test 超时") from error
        output = result.stdout.strip()
        if result.returncode != 0 or not output.startswith("cpagw "):
            raise VerificationError(
                "原生 binary version smoke test 失败："
                f"exit={result.returncode} stdout={output!r} stderr={result.stderr.strip()!r}"
            )


def verify(dist: Path, run_native_smoke: bool = True) -> None:
    if not dist.is_dir():
        raise VerificationError(f"产物目录不存在：{dist}")
    expected_files = set(ARCHIVES) | {"checksums.txt"}
    metadata_files = {"artifacts.json", "config.yaml", "metadata.json"}
    allowed_files = expected_files | metadata_files | BUILD_OUTPUTS
    entries = list(dist.iterdir())
    actual_files = {path.name for path in entries if path.is_file()}
    unexpected = {
        path.name
        for path in entries
        if path.is_symlink()
        or (path.is_file() and path.name not in allowed_files)
        or (path.is_dir() and path.name not in BUILD_OUTPUTS)
        or (not path.is_file() and not path.is_dir())
    }
    missing = expected_files - actual_files
    if missing or unexpected:
        raise VerificationError(
            f"产物文件清单不匹配；缺少={sorted(missing)}，多余={sorted(unexpected)}"
        )
    for name in BUILD_OUTPUTS & {path.name for path in entries if path.is_dir()}:
        build_dir = dist / name
        binary = build_dir / "cpagw"
        if build_dir.is_symlink() or [item.name for item in build_dir.iterdir()] != ["cpagw"]:
            raise VerificationError(f"GoReleaser 构建目录结构异常：{name}")
        if binary.is_symlink() or not binary.is_file() or binary.stat().st_size <= 0:
            raise VerificationError(f"GoReleaser 构建 binary 缺失或无效：{name}")
    for name in ARCHIVES:
        path = dist / name
        if path.is_symlink():
            raise VerificationError(f"归档不能是符号链接：{name}")
        verify_archive(path)
    verify_checksums(dist)
    if run_native_smoke:
        verify_native_binary(dist)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("dist", type=Path, help="GoReleaser dist 目录")
    parser.add_argument(
        "--skip-native-smoke",
        action="store_true",
        help="只校验归档与校验和，不执行当前平台 binary（供测试使用）",
    )
    args = parser.parse_args()

    try:
        verify(args.dist, run_native_smoke=not args.skip_native_smoke)
    except (OSError, VerificationError, subprocess.SubprocessError) as error:
        print(f"发布产物验证失败：{error}", file=sys.stderr)
        return 1
    if args.skip_native_smoke:
        print("四平台归档与 checksums.txt 校验通过；原生 binary smoke test 未执行")
    else:
        print("四平台归档、checksums.txt 与原生 binary smoke test 均通过")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
