#!/usr/bin/env python3
"""覆盖发布产物验证器的成功与 fail-closed 场景。"""

from __future__ import annotations

import contextlib
import hashlib
import importlib.util
import io
import subprocess
import struct
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest import mock

SCRIPT = Path(__file__).with_name("verify-release-artifacts.py")
SPEC = importlib.util.spec_from_file_location("release_artifact_verifier", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"无法加载发布产物验证脚本：{SCRIPT}")
VERIFIER = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = VERIFIER
SPEC.loader.exec_module(VERIFIER)
ARCHIVES = (
    "cpagw_darwin_amd64.tar.gz",
    "cpagw_darwin_arm64.tar.gz",
    "cpagw_linux_amd64.tar.gz",
    "cpagw_linux_arm64.tar.gz",
)


def binary_header(name: str) -> bytes:
    header = bytearray(32)
    arm64 = "arm64" in name
    if "darwin" in name:
        header[:4] = b"\xcf\xfa\xed\xfe"
        struct.pack_into("<I", header, 4, 0x0100000C if arm64 else 0x01000007)
    else:
        header[:7] = b"\x7fELF\x02\x01\x01"
        struct.pack_into("<H", header, 18, 183 if arm64 else 62)
    return bytes(header)


class ReleaseArtifactVerifierTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="cpagw-artifact-test-")
        self.dist = Path(self.temporary.name) / "dist"
        self.dist.mkdir()
        self.write_archives()

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def write_archives(self, malformed: str | None = None) -> None:
        for name in ARCHIVES:
            path = self.dist / name
            with tarfile.open(path, mode="w:gz") as archive:
                files = {
                    "cpagw": binary_header(name),
                    "README.md": b"readme",
                    "THIRD_PARTY_NOTICES": b"notices",
                }
                if name == ARCHIVES[0] and malformed == "missing":
                    files.pop("README.md")
                if name == ARCHIVES[0] and malformed == "traversal":
                    files["../unexpected"] = b"outside"
                for member_name, content in files.items():
                    info = tarfile.TarInfo(member_name)
                    info.size = len(content)
                    info.mode = 0o755 if member_name == "cpagw" else 0o644
                    archive.addfile(info, io.BytesIO(content))
        self.write_checksums()

    def write_checksums(self) -> None:
        lines = []
        for name in ARCHIVES:
            digest = hashlib.sha256((self.dist / name).read_bytes()).hexdigest()
            lines.append(f"{digest}  {name}")
        (self.dist / "checksums.txt").write_text("\n".join(lines) + "\n", encoding="ascii")

    def run_verifier(self) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sys.executable, "-I", str(SCRIPT), str(self.dist), "--skip-native-smoke"],
            check=False,
            capture_output=True,
            text=True,
        )

    def test_accepts_complete_archives_and_checksums(self) -> None:
        result = self.run_verifier()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("原生 binary smoke test 未执行", result.stdout)

    def run_main_with_process(self, process_result: object) -> tuple[int, str]:
        stderr = io.StringIO()
        process_patch = (
            {"side_effect": process_result}
            if isinstance(process_result, BaseException)
            else {"return_value": process_result}
        )
        with mock.patch.object(VERIFIER.subprocess, "run", **process_patch):
            with mock.patch.object(VERIFIER.sys, "argv", [str(SCRIPT), str(self.dist)]):
                with contextlib.redirect_stderr(stderr):
                    exit_code = VERIFIER.main()
        return exit_code, stderr.getvalue()

    def test_native_smoke_uses_isolated_home_and_state_dir(self) -> None:
        if VERIFIER.native_target() is None:
            self.skipTest("当前平台不支持原生 GoReleaser 目标")
        result = subprocess.CompletedProcess([], 0, stdout="cpagw test\n", stderr="")
        with mock.patch.object(VERIFIER.subprocess, "run", return_value=result) as run:
            VERIFIER.verify_native_binary(self.dist)
        argv = run.call_args.args[0]
        environment = run.call_args.kwargs["env"]
        state_dir = Path(argv[argv.index("--state-dir") + 1])
        temporary_root = state_dir.parent
        self.assertEqual(argv[-1], "version")
        self.assertEqual(len(argv), 4)
        self.assertEqual(Path(environment["HOME"]), temporary_root / "home")
        self.assertEqual(Path(environment["XDG_CONFIG_HOME"]), temporary_root / "config")
        self.assertTrue(temporary_root in state_dir.parents)
        self.assertNotIn("--api-key-stdin", argv)

    def test_native_smoke_reports_stdout_exit_and_timeout_failures(self) -> None:
        if VERIFIER.native_target() is None:
            self.skipTest("当前平台不支持原生 GoReleaser 目标")
        cases = (
            (subprocess.CompletedProcess([], 0, stdout="unexpected output", stderr=""), "unexpected output"),
            (subprocess.CompletedProcess([], 1, stdout="", stderr="failed"), "exit=1"),
            (subprocess.TimeoutExpired(cmd="cpagw", timeout=20), "smoke test 超时"),
        )
        for process_result, expected_error in cases:
            with self.subTest(expected_error=expected_error):
                exit_code, stderr = self.run_main_with_process(process_result)
                self.assertEqual(exit_code, 1)
                self.assertIn(expected_error, stderr)

    def test_rejects_missing_archive(self) -> None:
        (self.dist / ARCHIVES[-1]).unlink()
        result = self.run_verifier()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("产物文件清单不匹配", result.stderr)

    def test_rejects_missing_checksums_file(self) -> None:
        (self.dist / "checksums.txt").unlink()
        result = self.run_verifier()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("产物文件清单不匹配", result.stderr)

    def test_rejects_corrupt_checksum(self) -> None:
        path = self.dist / "checksums.txt"
        lines = path.read_text(encoding="ascii").splitlines()
        lines[0] = ("0" if lines[0][0] != "0" else "1") + lines[0][1:]
        path.write_text("\n".join(lines) + "\n", encoding="ascii")
        result = self.run_verifier()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SHA256 不匹配", result.stderr)

    def test_rejects_malformed_package_members(self) -> None:
        self.write_archives(malformed="missing")
        result = self.run_verifier()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("归档文件清单不匹配", result.stderr)

    def test_rejects_package_path_traversal(self) -> None:
        self.write_archives(malformed="traversal")
        result = self.run_verifier()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("归档文件清单不匹配", result.stderr)


if __name__ == "__main__":
    unittest.main()
