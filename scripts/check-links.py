#!/usr/bin/env python3
"""Check internal Markdown links and heading anchors in the docs.

Validates relative file links and same-file/cross-file `#anchor` links (using the
GitHub heading-slug algorithm) across README.md and docs/*.md. External http(s)
links are listed but not fetched. Exit non-zero if any internal link is broken.
"""
import os
import re
import sys
import glob

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FILES = ["README.md"] + sorted(glob.glob(os.path.join(ROOT, "docs", "*.md")))

LINK = re.compile(r"\[([^\]]*)\]\(([^)]+)\)")
HEADING = re.compile(r"^(#{1,6})\s+(.*?)\s*$")


def slug(title):
    s = title.strip().lower()
    s = s.replace("`", "")
    s = re.sub(r"[^\w\s-]", "", s)
    s = s.replace(" ", "-")
    return s


def anchors_for(path):
    anchors, seen = set(), {}
    with open(path, encoding="utf-8") as f:
        for line in f:
            m = HEADING.match(line)
            if not m:
                continue
            base = slug(m.group(2))
            c = seen.get(base, 0)
            anchors.add(base if c == 0 else f"{base}-{c}")
            seen[base] = c + 1
    return anchors


def main():
    broken, external, ok = [], [], 0
    cache = {}
    for rel in FILES:
        path = rel if os.path.isabs(rel) else os.path.join(ROOT, rel)
        disp = os.path.relpath(path, ROOT)
        with open(path, encoding="utf-8") as f:
            text = f.read()
        for _label, target in LINK.findall(text):
            target = target.strip()
            if target.startswith(("http://", "https://", "mailto:")):
                external.append((disp, target))
                continue
            p, anchor = (target.split("#", 1) + [None])[:2] if "#" in target else (target, None)
            tgt = path if p == "" else os.path.normpath(os.path.join(os.path.dirname(path), p))
            if p != "" and not os.path.exists(tgt):
                broken.append((disp, target, "file not found"))
                continue
            if anchor is not None:
                if tgt not in cache:
                    cache[tgt] = anchors_for(tgt) if os.path.exists(tgt) else set()
                if anchor not in cache[tgt]:
                    broken.append((disp, target, f"anchor '#{anchor}' not found"))
                    continue
            ok += 1

    print(f"Scanned {len(FILES)} files. Internal links OK: {ok}. "
          f"Broken: {len(broken)}. External (not fetched): {len(external)}.")
    if broken:
        print("\nBROKEN:")
        for rel, t, why in broken:
            print(f"  {rel}: [{t}] -> {why}")
        return 1
    print("No broken internal links or anchors.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
