from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

from patch_note import parse_sections, run_patch, sha256

SCRIPT = Path(__file__).with_name("patch_note.py")
VALID_NOTE = '''---
domain: [server]
question: "어떤 질문에 답하는 새 문서인가?"
owner: server-team
verification: chat-derived
---

# 새 문서
'''


class WikiPatchTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name) / "wiki"
        self.root.mkdir()
        subprocess.run(["git", "init", "-q", str(self.root)], check=True)
        (self.root / "docs").mkdir()
        self.path = self.root / "docs" / "note.md"
        self.content = (
            "---\r\n"
            "domain: [server]\r\n"
            'question: "배포 절차는 무엇인가?"\r\n'
            "owner: server-team\r\n"
            "verification: code-verified\r\n"
            "---\r\n\r\n"
            "# 운영\r\n\r\n"
            "## 반복 제목\r\n첫 절\r\n\r\n"
            "## 대상\r\n기존 내용\r\n\r\n"
            "## 반복 제목\r\n둘째 절\r\n"
        )
        self.path.write_bytes(self.content.encode("utf-8"))

    def tearDown(self) -> None:
        self.temp.cleanup()

    def patch(self, **updates):
        sections = parse_sections(self.content)
        target = next(section for section in sections if section["heading"] == "대상")
        patch = {
            "path": "docs/note.md",
            "expected_note_hash": sha256(self.content),
            "operations": [{
                "section_id": target["section_id"],
                "expected_hash": target["hash"],
                "replacement": "## 대상\r\n새 내용\r\n\r\n",
            }],
        }
        patch.update(updates)
        return patch

    def test_dry_run_reports_hash_and_diff_without_writing(self):
        result = run_patch(str(self.root), self.patch())
        self.assertFalse(result["applied"])
        self.assertEqual(result["before_hash"], sha256(self.content))
        self.assertIn("+새 내용", result["diff"])
        self.assertEqual(self.path.read_bytes(), self.content.encode("utf-8"))

    def test_apply_preserves_frontmatter_neighbors_and_crlf(self):
        result = run_patch(str(self.root), self.patch(), apply=True)
        after = self.path.read_bytes().decode("utf-8")
        self.assertTrue(result["applied"])
        self.assertTrue(after.startswith(self.content[:self.content.index("# 운영")]))
        self.assertIn("## 반복 제목\r\n첫 절\r\n\r\n", after)
        self.assertIn("## 반복 제목\r\n둘째 절\r\n", after)
        self.assertIn("## 대상\r\n새 내용\r\n\r\n", after)
        self.assertNotIn("기존 내용", after)

    def test_stale_note_hash_rejects_without_writing(self):
        original = self.path.read_bytes()
        with self.assertRaisesRegex(ValueError, "Stale note hash"):
            run_patch(str(self.root), self.patch(expected_note_hash="0" * 64), apply=True)
        self.assertEqual(self.path.read_bytes(), original)

    def test_stale_section_hash_rejects_without_writing(self):
        patch = self.patch()
        patch["operations"][0]["expected_hash"] = "0" * 64
        original = self.path.read_bytes()
        with self.assertRaisesRegex(ValueError, "Stale section hash"):
            run_patch(str(self.root), patch, apply=True)
        self.assertEqual(self.path.read_bytes(), original)

    def test_duplicate_section_operation_is_rejected(self):
        patch = self.patch()
        patch["operations"].append(dict(patch["operations"][0]))
        with self.assertRaisesRegex(ValueError, "Duplicate section IDs"):
            run_patch(str(self.root), patch)

    def test_duplicate_heading_ids_remain_distinct(self):
        sections = [section for section in parse_sections(self.content) if section["heading"] == "반복 제목"]
        self.assertEqual([section["section_id"] for section in sections], ["운영-반복-제목:1", "운영-반복-제목:2"])

    def test_traversal_and_symlink_paths_are_rejected(self):
        with self.assertRaisesRegex(ValueError, "stay inside the checkout"):
            run_patch(str(self.root), {"path": "../outside.md", "create": VALID_NOTE})
        outside = Path(self.temp.name) / "outside"
        outside.mkdir()
        link = self.root / "linked"
        try:
            link.symlink_to(outside, target_is_directory=True)
        except (OSError, NotImplementedError) as error:
            self.skipTest(f"symlinks unavailable: {error}")
        with self.assertRaisesRegex(ValueError, "symlinks"):
            run_patch(str(self.root), {"path": "linked/note.md", "create": VALID_NOTE})

    def test_create_requires_metadata_and_never_overwrites(self):
        with self.assertRaisesRegex(ValueError, "frontmatter"):
            run_patch(str(self.root), {"path": "docs/new.md", "create": "# no frontmatter\n"})
        new_path = self.root / "docs" / "new.md"
        result = run_patch(str(self.root), {"path": "docs/new.md", "create": VALID_NOTE}, apply=True)
        self.assertTrue(result["applied"])
        self.assertEqual(new_path.read_text(encoding="utf-8"), VALID_NOTE)
        with self.assertRaisesRegex(ValueError, "Create collision"):
            run_patch(str(self.root), {"path": "docs/new.md", "create": VALID_NOTE})

    def test_cli_defaults_to_dry_run_and_requires_apply_to_write(self):
        patch = self.patch()
        input_path = Path(self.temp.name) / "patch.json"
        input_path.write_text(json.dumps(patch), encoding="utf-8")
        args = [sys.executable, str(SCRIPT), "--root", str(self.root), "--input", str(input_path)]
        dry_run = subprocess.run(args, text=True, capture_output=True, check=True)
        self.assertFalse(json.loads(dry_run.stdout)["applied"])
        self.assertEqual(self.path.read_bytes(), self.content.encode("utf-8"))
        applied = subprocess.run([*args, "--apply"], text=True, capture_output=True, check=True)
        self.assertTrue(json.loads(applied.stdout)["applied"])
        self.assertIn("새 내용", self.path.read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()
