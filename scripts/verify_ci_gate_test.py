#!/usr/bin/env python3
"""验证合并门禁只接受所有依赖 job 均成功的结果。"""

from __future__ import annotations

import subprocess
import unittest
from pathlib import Path

SCRIPT = Path(__file__).with_name("verify-ci-gate.sh")


class CIGateTest(unittest.TestCase):
    def run_gate(self, *results: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["bash", str(SCRIPT), *results],
            check=False,
            capture_output=True,
            text=True,
        )

    def test_accepts_only_all_success(self) -> None:
        result = self.run_gate("success", "success", "success")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_rejects_failure_cancelled_skipped_and_unknown(self) -> None:
        for rejected in ("failure", "cancelled", "skipped", "unknown", ""):  # fail closed
            with self.subTest(result=rejected):
                result = self.run_gate("success", rejected, "success")
                self.assertNotEqual(result.returncode, 0)

    def test_rejects_missing_dependency_results(self) -> None:
        result = self.run_gate()
        self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
