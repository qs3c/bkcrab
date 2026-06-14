import re, os, sys

DIRS = [r"E:\\fromGithub\\bkclaw\\internal\\setup", r"E:\\fromGithub\\bkclaw\\internal\\skills"]
OUT = open(r"E:\\fromGithub\\bkclaw\\all_comments.txt", "w", encoding="utf-8")

files = []
for d in DIRS:
    for root, dirs, fnames in os.walk(d):
        for fn in fnames:
            if fn.endswith('.go') and not fn.endswith('_test.go'):
                files.append(os.path.join(root, fn))

OUT.write(f"Found {len(files)} files\n\n")

for fp in files:
    OUT.write(f"\n=== {fp} ===\n")
    with open(fp, 'r', encoding='utf-8') as f:
        lines = f.read().split('\n')
    for i, line in enumerate(lines):
        stripped = line.strip()
        if stripped.startswith('//') and not stripped.startswith('//go:') and not stripped.startswith('//nolint'):
            comment = stripped[2:].strip()
            if comment and any(c.isalpha() for c in comment):
                OUT.write(f"  L{i+1}: {comment}\n")

OUT.close()
print("Done")
