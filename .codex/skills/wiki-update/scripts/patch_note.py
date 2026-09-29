#!/usr/bin/env python3
"""Apply section-hash-guarded patches to a local Framework Wiki checkout."""
from __future__ import annotations

import argparse
import difflib
import hashlib
import json
import os
import re
import stat
import subprocess
import sys
import tempfile
import time
import unicodedata
from pathlib import Path, PurePosixPath
from typing import Any

HASH_RE = re.compile(r"^[a-f0-9]{64}$")
FRONTMATTER_RE = re.compile(r"^(?:\ufeff)?---\r?\n([\s\S]*?)\r?\n---(?:\r?\n|$)")
FENCE_RE = re.compile(r"^ {0,3}(`{3,}|~{3,})(.*)$")
ATX_RE = re.compile(r"^ {0,3}(#{1,6})\s+(.+?)\s*$")
SETEXT_RE = re.compile(r"^ {0,3}(=+|-+)\s*$")
ALLOWED_DOMAINS = {"product", "server", "ai", "client", "shared"}
ALLOWED_VERIFICATION = {
    "runtime-verified", "code-verified", "source-verified", "team-confirmed",
    "mixed-verified", "partial", "chat-derived", "unverified",
}


def sha256(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def _line_rows(content: str) -> list[tuple[str, int, int, int]]:
    rows: list[tuple[str, int, int, int]] = []
    pattern = re.compile(r"[^\r\n]*(?:\r\n|\n|\r|$)")
    for match in pattern.finditer(content):
        raw = match.group(0)
        if not raw:
            break
        text = raw.rstrip("\r\n")
        rows.append((text, match.start(), match.end(), len(rows) + 1))
    return rows


def _frontmatter_offset(content: str) -> int:
    match = FRONTMATTER_RE.match(content)
    return match.end() if match else 0


def _heading_text(value: str) -> str:
    return re.sub(r"\s+#+\s*$", "", value).strip()


def _slug(value: str) -> str:
    value = unicodedata.normalize("NFKC", value).lower()
    result: list[str] = []
    replacing = False
    for char in value:
        category = unicodedata.category(char)
        if category[0] in "LN" or char in "_-":
            result.append(char)
            replacing = False
        elif not replacing:
            result.append("-")
            replacing = True
    slug = "".join(result).strip("-")
    return slug or "section"


def parse_sections(content: str) -> list[dict[str, Any]]:
    """Mirror MCP heading sections and IDs so outline hashes address the same text."""
    offset = _frontmatter_offset(content)
    lines = _line_rows(content)
    starts: list[dict[str, Any]] = []
    ancestors: list[tuple[int, str]] = []
    fence: tuple[str, int] | None = None
    index = 0
    while index < len(lines):
        line, start, _end, number = lines[index]
        if start < offset:
            index += 1
            continue
        marker = FENCE_RE.match(line)
        if fence:
            if marker and marker.group(1)[0] == fence[0] and len(marker.group(1)) >= fence[1] and not marker.group(2).strip():
                fence = None
            index += 1
            continue
        if marker:
            fence = (marker.group(1)[0], len(marker.group(1)))
            index += 1
            continue

        atx = ATX_RE.match(line)
        next_line = lines[index + 1][0] if index + 1 < len(lines) else ""
        setext = SETEXT_RE.match(next_line) if not atx and line.strip() else None
        valid_setext = bool(setext and not re.match(r"^(?: {4}|\t|\s*[>|*+-]\s|\s*\|)", line))
        if not atx and not valid_setext:
            index += 1
            continue

        level = len(atx.group(1)) if atx else (1 if setext and setext.group(1)[0] == "=" else 2)
        heading = _heading_text(atx.group(2) if atx else line)
        while ancestors and ancestors[-1][0] >= level:
            ancestors.pop()
        ancestors.append((level, heading))
        starts.append({"start": start, "line": number, "heading": heading,
                       "level": level, "headings": [item[1] for item in ancestors]})
        index += 2 if valid_setext else 1

    if not starts or starts[0]["start"] > offset:
        preamble_line = len(re.split(r"\r?\n", content[:offset]))
        starts.insert(0, {"start": offset, "line": preamble_line, "heading": "", "level": 0, "headings": []})

    occurrences: dict[str, int] = {}
    sections = []
    for index, entry in enumerate(starts):
        end = starts[index + 1]["start"] if index + 1 < len(starts) else len(content)
        section_content = content[entry["start"]:end]
        base = _slug("/".join(entry["headings"])) if entry["heading"] else "preamble"
        occurrences[base] = occurrences.get(base, 0) + 1
        if section_content:
            sections.append({
                "section_id": f"{base}:{occurrences[base]}",
                "heading": entry["heading"],
                "headings": entry["headings"],
                "level": entry["level"],
                "content": section_content,
                "hash": sha256(section_content),
                "start": entry["start"],
                "end": end,
            })
    return sections


def validate_new_note(content: str) -> None:
    match = FRONTMATTER_RE.match(content)
    if not match or not content.startswith("---"):
        raise ValueError("New notes must start with YAML frontmatter.")
    metadata_lines = match.group(1).splitlines()
    fields: dict[str, tuple[int, str]] = {}
    for index, line in enumerate(metadata_lines):
        field = re.match(r"^(domain|question|owner|verification):\s*(.*?)\s*$", line)
        if field:
            fields[field.group(1)] = (index, field.group(2))
    required = ("domain", "question", "owner", "verification")
    if any(key not in fields for key in required):
        raise ValueError("New notes require domain, question, owner, verification frontmatter.")
    if [fields[key][0] for key in required] != sorted(fields[key][0] for key in required):
        raise ValueError("Frontmatter fields must appear in domain, question, owner, verification order.")
    domain_value = fields["domain"][1]
    domain_match = re.fullmatch(r"\[\s*([a-z-]+(?:\s*,\s*[a-z-]+)*)\s*\]", domain_value)
    if not domain_match or any(value.strip() not in ALLOWED_DOMAINS for value in domain_match.group(1).split(",")):
        raise ValueError("domain must be an array of allowed wiki domains.")
    question_value = fields["question"][1]
    if len(question_value) < 3 or question_value[0] != '"' or question_value[-2:-1] != "?" or question_value[-1] != '"':
        raise ValueError("question must be a double-quoted question ending with ?.")
    if not fields["owner"][1] or any(ch.isspace() for ch in fields["owner"][1]):
        raise ValueError("owner must be one non-empty team identifier.")
    if fields["verification"][1] not in ALLOWED_VERIFICATION:
        raise ValueError("verification must use a supported wiki status.")


def _validate_path(root: Path, relative: Any, creating: bool) -> Path:
    if not isinstance(relative, str) or not relative or "\\" in relative:
        raise ValueError("path must be a non-empty relative Markdown path.")
    raw_parts = relative.split("/")
    posix = PurePosixPath(relative)
    if posix.is_absolute() or any(part in {"", ".", ".."} for part in raw_parts) or posix.suffix.lower() != ".md":
        raise ValueError("path must stay inside the checkout and end in .md.")
    target = root.joinpath(*posix.parts)
    try:
        target.relative_to(root)
    except ValueError as exc:
        raise ValueError("path escapes the checkout.") from exc
    current = root
    for index, part in enumerate(posix.parts):
        current = current / part
        try:
            mode = current.lstat().st_mode
        except FileNotFoundError:
            if index < len(posix.parts) - 1:
                raise ValueError("Parent directories must already exist.")
            if not creating:
                raise ValueError("Wiki note does not exist.")
            break
        if stat.S_ISLNK(mode):
            raise ValueError("Patch paths cannot contain symlinks.")
        if index < len(posix.parts) - 1 and not stat.S_ISDIR(mode):
            raise ValueError("Parent path component is not a directory.")
    resolved_parent = target.parent.resolve(strict=True)
    if root != resolved_parent and root not in resolved_parent.parents:
        raise ValueError("path escapes the checkout.")
    return target


def _checkout_root(root_arg: str) -> Path:
    root = Path(root_arg).resolve(strict=True)
    result = subprocess.run(["git", "-C", str(root), "rev-parse", "--show-toplevel"],
                            text=True, capture_output=True, check=False)
    if result.returncode:
        raise ValueError("Patch root must be a local Git checkout root.")
    top = Path(result.stdout.strip()).resolve(strict=True)
    if top != root:
        raise ValueError("Patch root must be the Git checkout root, not a subdirectory.")
    return root


def _validate_request(value: Any) -> None:
    if not isinstance(value, dict) or not isinstance(value.get("path"), str):
        raise ValueError("Invalid wiki patch request.")
    if "create" in value:
        if not isinstance(value["create"], str) or not value["create"].strip() or "operations" in value or "expected_note_hash" in value:
            raise ValueError("Create patches require only path and non-empty create content.")
        validate_new_note(value["create"])
        return
    operations = value.get("operations")
    note_hash = value.get("expected_note_hash")
    if not HASH_RE.fullmatch(note_hash or ""):
        raise ValueError("Section patches require a 64-character expected_note_hash.")
    if not isinstance(operations, list) or not operations or len(operations) > 64:
        raise ValueError("Section patches require 1 to 64 operations.")
    section_ids = []
    for operation in operations:
        if (not isinstance(operation, dict) or not isinstance(operation.get("section_id"), str)
                or not HASH_RE.fullmatch(operation.get("expected_hash") or "")
                or not isinstance(operation.get("replacement"), str)):
            raise ValueError("Each operation requires section_id, expected_hash, and replacement.")
        section_ids.append(operation["section_id"])
    if len(set(section_ids)) != len(section_ids):
        raise ValueError("Duplicate section IDs are not allowed.")


def _compact_diff(before: str, after: str, limit: int = 2000) -> tuple[str, bool]:
    before_lines, after_lines = before.split("\n"), after.split("\n")
    prefix = 0
    while prefix < min(len(before_lines), len(after_lines)) and before_lines[prefix] == after_lines[prefix]:
        prefix += 1
    suffix = 0
    while (suffix < len(before_lines) - prefix and suffix < len(after_lines) - prefix
           and before_lines[-suffix - 1] == after_lines[-suffix - 1]):
        suffix += 1
    removed = before_lines[prefix:len(before_lines) - suffix if suffix else len(before_lines)]
    added = after_lines[prefix:len(after_lines) - suffix if suffix else len(after_lines)]
    diff = "\n".join([f"@@ -{prefix + 1},{len(removed)} +{prefix + 1},{len(added)} @@",
                       *[f"-{line}" for line in removed], *[f"+{line}" for line in added]])
    return diff[:limit], len(diff) > limit


def _prepare(root: Path, request: dict[str, Any]) -> tuple[Path, str, str, int, dict[str, Any]]:
    _validate_request(request)
    creating = "create" in request
    target = _validate_path(root, request["path"], creating)
    before = ""
    mode = 0o644
    if target.exists():
        if target.is_symlink() or not target.is_file():
            raise ValueError("Patch target must be a regular Markdown file.")
        if creating:
            raise ValueError("Create collision: wiki note already exists.")
        before_bytes = target.read_bytes()
        before = before_bytes.decode("utf-8")
        mode = stat.S_IMODE(target.stat().st_mode)
    elif not creating:
        raise ValueError("Wiki note does not exist.")

    before_hash = None if creating else sha256(before)
    if creating:
        after = request["create"]
        changed = ["create"]
    else:
        if before_hash != request["expected_note_hash"]:
            raise ValueError("Stale note hash: the wiki note changed after the proposal.")
        sections = parse_sections(before)
        by_id = {section["section_id"]: section for section in sections}
        replacements = []
        for operation in request["operations"]:
            section = by_id.get(operation["section_id"])
            if section is None:
                raise ValueError(f"Wiki section not found: {operation['section_id']}")
            if section["hash"] != operation["expected_hash"]:
                raise ValueError(f"Stale section hash: {operation['section_id']}")
            replacements.append((section, operation))
        replacements.sort(key=lambda pair: pair[0]["start"], reverse=True)
        after = before
        for section, operation in replacements:
            after = after[:section["start"]] + operation["replacement"] + after[section["end"]:]
        changed = [operation["section_id"] for operation in request["operations"]]
        old_frontmatter = FRONTMATTER_RE.match(before)
        if old_frontmatter and not after.startswith(old_frontmatter.group(0)):
            raise ValueError("Section patch cannot alter existing frontmatter.")

    after_hash = sha256(after)
    diff, truncated = _compact_diff(before, after)
    preview = {
        "path": request["path"],
        "action": "create" if creating else "replace",
        "before_hash": before_hash,
        "after_hash": after_hash,
        "changed_sections": changed,
        "before_chars": len(before),
        "after_chars": len(after),
        "diff": diff,
        "diff_truncated": truncated,
        "applied": False,
    }
    return target, before, after, mode, preview


def _lock_path(root: Path) -> Path:
    result = subprocess.run(["git", "-C", str(root), "rev-parse", "--git-path", "framework-wiki-patch.lock"],
                            text=True, capture_output=True, check=True)
    lock = Path(result.stdout.strip())
    return lock if lock.is_absolute() else root / lock


def run_patch(root_arg: str, request: dict[str, Any], apply: bool = False) -> dict[str, Any]:
    root = _checkout_root(root_arg)
    if not apply:
        return _prepare(root, request)[4]

    lock_path = _lock_path(root)
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    flags = os.O_CREAT | os.O_EXCL | os.O_WRONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        lock_fd = os.open(lock_path, flags, 0o600)
    except FileExistsError as exc:
        raise ValueError("Another local wiki patch is in progress. Inspect the lock before retrying.") from exc

    temporary: Path | None = None
    try:
        os.write(lock_fd, f"pid={os.getpid()} time={int(time.time())}\n".encode())
        os.fsync(lock_fd)
        target, before, after, mode, preview = _prepare(root, request)
        current = _validate_path(root, request["path"], preview["action"] == "create")
        if current != target:
            raise ValueError("Patch path changed while applying.")
        with tempfile.NamedTemporaryFile(prefix=f".{target.name}.wiki-patch-", dir=target.parent, delete=False) as handle:
            temporary = Path(handle.name)
            os.fchmod(handle.fileno(), mode)
            handle.write(after.encode("utf-8"))
            handle.flush()
            os.fsync(handle.fileno())

        _validate_path(root, request["path"], preview["action"] == "create")
        if preview["action"] == "create":
            os.link(temporary, target)
            temporary.unlink()
            temporary = None
        else:
            current_text = target.read_bytes().decode("utf-8")
            if sha256(current_text) != preview["before_hash"]:
                raise ValueError("Stale note hash: the wiki changed while applying the patch.")
            os.replace(temporary, target)
            temporary = None
        directory_fd = os.open(target.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
        preview["applied"] = True
        return preview
    finally:
        if temporary is not None:
            try:
                temporary.unlink()
            except FileNotFoundError:
                pass
        os.close(lock_fd)
        try:
            lock_path.unlink()
        except FileNotFoundError:
            pass


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, help="Root of a local wiki Git checkout")
    parser.add_argument("--input", required=True, help="JSON patch request")
    parser.add_argument("--apply", action="store_true", help="Write the patch; default is dry-run")
    args = parser.parse_args(argv)
    try:
        request = json.loads(Path(args.input).read_text(encoding="utf-8"))
        result = run_patch(args.root, request, apply=args.apply)
        print(json.dumps(result, ensure_ascii=False))
        return 0
    except (OSError, UnicodeError, json.JSONDecodeError, ValueError, subprocess.CalledProcessError) as error:
        print(str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
