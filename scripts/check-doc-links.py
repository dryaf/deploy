#!/usr/bin/env python3
"""Check repository-local Markdown links in README.md and docs/ (no network)."""

import re
from pathlib import Path
from urllib.parse import unquote, urlsplit


ROOT = Path(__file__).resolve().parent.parent
DOCUMENTS = [ROOT / "README.md", *sorted((ROOT / "docs").rglob("*.md"))]


def prose(path):
    fence = None
    for number, line in enumerate(path.read_text().splitlines(), 1):
        marker = re.match(r"^\s{0,3}(`{3,}|~{3,})", line)
        if marker:
            value = marker[1]
            if fence is None:
                fence = value
            elif value[0] == fence[0] and len(value) >= len(fence):
                fence = None
            continue
        if fence is None:
            yield number, line


def anchors(path):
    result, counts = set(), {}
    for _, line in prose(path):
        heading = re.match(r"^#{1,6}\s+(.+?)(?:\s+#+)?$", line)
        if heading:
            title = re.sub(r"\[([^\]]+)\]\([^)]*\)", r"\1", heading[1])
            title = re.sub(r"<[^>]+>", "", title)
            slug = re.sub(r"[^\w\- ]", "", title.lower()).replace(" ", "-")
            count = counts.get(slug, 0)
            counts[slug] = count + 1
            result.add(slug + (f"-{count}" if count else ""))
        result.update(re.findall(r'\b(?:id|name)=["\x27]([^"\x27]+)["\x27]', line))
    return result


def main():
    errors, checked = [], 0
    anchor_cache = {}
    for doc in DOCUMENTS:
        # Join prose lines so wrapped labels remain valid links.
        lines = list(prose(doc))
        body = "\n".join(line for _, line in lines)
        for match in re.finditer(r"\[[^\]]*\]\(([^\s)]+)(?:\s+\"[^\"]*\")?\)", body):
            href = match[1].strip("<>")
            url = urlsplit(href)
            if url.scheme or url.netloc:
                continue
            line = lines[body.count("\n", 0, match.start())][0]
            target = (doc.parent / unquote(url.path)).resolve() if url.path else doc
            where = f"{doc.relative_to(ROOT)}:{line}: {href}"
            checked += 1
            if not target.exists():
                errors.append(f"{where}: missing target")
            elif url.fragment and target.suffix.lower() == ".md":
                if target not in anchor_cache:
                    anchor_cache[target] = anchors(target)
                if unquote(url.fragment) not in anchor_cache[target]:
                    errors.append(f"{where}: missing heading/anchor")
    if errors:
        print("\n".join(errors))
        return 1
    print(f"Checked {checked} local links in {len(DOCUMENTS)} Markdown files.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
