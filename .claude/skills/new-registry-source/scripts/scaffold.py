#!/usr/bin/env python3
"""Scaffold sources/NAME for dug and wire it into the workspace.

Usage: python3 scaffold.py NAME [--title "Open VSX"] [--port 7786] [--root DIR]

Creates the module from compiling templates (TODO(NAME) markers show where
registry facts go) and applies every registration edit that has a stable
anchor. Each edit is reported as edited, already present, or not found; a
not-found edit is left for you, see references/wiring.md. Nothing outside the
reported files is touched, and the script never overwrites an existing module.
"""
import argparse
import pathlib
import re
import shutil
import sys

TEMPLATES = pathlib.Path(__file__).resolve().parent / "templates"
EDITED, PRESENT, NOT_FOUND, MISSING = "edited", "already present", "NOT FOUND, do by hand", "file missing"


def find_root(explicit):
    if explicit:
        return pathlib.Path(explicit).resolve()
    starts = [pathlib.Path.cwd().resolve(), pathlib.Path(__file__).resolve()]
    for start in starts:
        for candidate in [start, *start.parents]:
            if (candidate / "go.work").is_file() and (candidate / "sources").is_dir():
                return candidate
    sys.exit("not inside the dug workspace (no go.work with sources/); pass --root")


def next_port(root):
    ports = [int(m.group(1)) for main in root.glob("sources/*/cmd/*/main.go")
             for m in re.finditer(r"127\.0\.0\.1:(\d{4,5})", main.read_text())]
    return max(ports) + 1 if ports else 7781


def render(template, values):
    text = (TEMPLATES / template).read_text()
    for key, value in values.items():
        text = text.replace("__" + key + "__", value)
    return text


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("name", help="registry identity, directory and executable suffix (lowercase, digits, hyphens)")
    parser.add_argument("--title", help="display name for docs and help text (default: capitalized NAME)")
    parser.add_argument("--port", type=int, help="default listen port (default: next free 778N)")
    parser.add_argument("--root", help="workspace root (default: found from cwd or this script)")
    args = parser.parse_args()

    name = args.name
    if not re.fullmatch(r"[a-z][a-z0-9-]{0,31}", name) or name.endswith("-"):
        sys.exit(f"invalid name {name!r}: lowercase letters, digits and inner hyphens only")
    pkg = name.replace("-", "")
    root = find_root(args.root)
    module = root / "sources" / name
    if module.exists():
        sys.exit(f"{module} already exists; this script only creates new sources")
    port = args.port or next_port(root)
    title = args.title or name[:1].upper() + name[1:]
    node = re.sub(r"[^A-Za-z0-9]", "", title) or pkg.capitalize()
    values = {"NAME": name, "PKG": pkg, "PORT": str(port), "TITLE": title}

    # Module files.
    created = []
    for template, target in [
        ("go.mod.tmpl", module / "go.mod"),
        ("source.go.tmpl", module / f"{pkg}.go"),
        ("plugin.go.tmpl", module / "plugin.go"),
        ("provider.go.tmpl", module / "provider.go"),
        ("source_test.go.tmpl", module / f"{pkg}_test.go"),
        ("README.md.tmpl", module / "README.md"),
        ("main.go.tmpl", module / "cmd" / f"dug-{name}" / "main.go"),
    ]:
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(render(template, values))
        created.append(target.relative_to(root))
    sums = sorted(root.glob("sources/*/go.sum"))
    donor = next((s for s in sums if s.parent.name == "firefox"), sums[0] if sums else None)
    if donor:
        shutil.copy(donor, module / "go.sum")
        created.append((module / "go.sum").relative_to(root))
    else:
        print("warning: no sources/*/go.sum to copy; run go mod tidy with a temporary SDK replace", file=sys.stderr)

    # Registration edits.
    results = []

    def apply(path, fn):
        file = root / path
        if not file.is_file():
            results.append((path, MISSING))
            return
        text = file.read_text()
        new, status = fn(text)
        if new != text:
            file.write_text(new)
        results.append((path, status))

    def gowork(text):
        m = re.search(r"^use \(\n(.*?)^\)", text, re.S | re.M)
        if not m:
            return text, NOT_FOUND
        entries = [line.strip() for line in m.group(1).splitlines() if line.strip()]
        entry = f"./sources/{name}"
        if entry in entries:
            return text, PRESENT
        entries = sorted(set(entries) | {entry})
        block = "use (\n" + "".join(f"\t{e}\n" for e in entries) + ")"
        return text[: m.start()] + block + text[m.end():], EDITED

    def build(text):
        line = f"go build -o bin/dug-{name} ./sources/{name}/cmd/dug-{name}"
        if line in text:
            return text, PRESENT
        lines = text.splitlines(keepends=True)
        idx = [i for i, l in enumerate(lines) if l.startswith("go build -o bin/dug-")]
        if not idx:
            return text, NOT_FOUND
        lines.insert(idx[-1] + 1, line + "\n")
        return "".join(lines), EDITED

    def check(text):
        m = re.search(r"^(\s*set -- \. sdk [^\n]*?) integration\s*$", text, re.M)
        if not m:
            return text, NOT_FOUND
        if f"sources/{name}" in m.group(1).split():
            return text, PRESENT
        return text[: m.start()] + f"{m.group(1)} sources/{name} integration" + text[m.end():], EDITED

    def boundaries(text):
        m = re.search(r"^(for module in \. sdk[^\n;]*); do\s*$", text, re.M)
        if not m:
            return text, NOT_FOUND
        if f"sources/{name}" in m.group(1).split():
            return text, PRESENT
        return text[: m.start()] + f"{m.group(1)} sources/{name}; do" + text[m.end():], EDITED

    def justfile(text):
        # The plugin recipe is a case statement; the arm that routes a source
        # to sources/$1/cmd/dug-$1 is either `NAMES) module="$1" ;;` or the
        # older `in NAMES) ;;`. Sources served from another module's directory
        # (wordpress-themes) have their own arm and are not touched.
        recipe = re.search(r"^plugin source \*args:\n((?:[ \t]+.*\n?)+)", text, re.M)
        if not recipe:
            return text, NOT_FOUND
        body = recipe.group(1)
        m = re.search(r'([a-z0-9|-]+)\) module="\$1" ;;', body) or re.search(r'@case "\$1" in ([a-z0-9|-]+)\) ;;', body)
        if not m:
            return text, NOT_FOUND
        names = m.group(1).split("|")
        if name in names or re.search(rf"(^|\|){re.escape(name)}\)", body):
            return text, PRESENT
        names.append(name)
        start = recipe.start(1) + m.start(1)
        text = text[:start] + "|".join(names) + text[start + len(m.group(1)):]
        comment = re.search(r"^(# Run one registry plugin in the foreground: )(.*?)\.?$", text, re.M)
        if comment:
            listed = [n.strip() for n in re.split(r",\s*(?:or\s+)?|\s+or\s+", comment.group(2)) if n.strip()]
            if name not in listed:
                listed.append(name)
            joined = ", ".join(listed[:-1]) + ", or " + listed[-1] if len(listed) > 2 else " or ".join(listed)
            text = text[: comment.start()] + comment.group(1) + joined + "." + text[comment.end():]
        return text, EDITED

    def help_text(text):
        if re.search(rf"^\S.*: dug-{re.escape(name)} \(", text, re.M):
            return text, PRESENT
        anchor = "Run a command with --help for transfer and storage options."
        i = text.find(anchor)
        if i < 0:
            return text, NOT_FOUND
        block = (f"{title}: dug-{name} (use --plugin-url http://127.0.0.1:{port})\n"
                 f"  --option names=NAME,NAME  TODO({name}): replace with the real options\n\n")
        return text[:i] + block + text[i:], EDITED

    def sources_readme(text):
        if f"| [{name}]({name}/) |" in text:
            return text, PRESENT
        lines = text.splitlines(keepends=True)
        rows = [i for i, l in enumerate(lines) if l.startswith("| [")]
        if not rows:
            return text, NOT_FOUND
        row = f"| [{name}]({name}/) | `{name}` | `127.0.0.1:{port}` | TODO({name}) | TODO({name}) |\n"
        lines.insert(rows[-1] + 1, row)
        return "".join(lines), EDITED

    def monorepo_table(text):
        if f"| `sources/{name}/` |" in text:
            return text, PRESENT
        lines = text.splitlines(keepends=True)
        rows = [i for i, l in enumerate(lines) if re.match(r"^\| `sources/[^`]+/` \|", l)]
        if not rows:
            return text, NOT_FOUND
        lines.insert(rows[-1] + 1, f"| `sources/{name}/` | {title} adapter, tests, `cmd/dug-{name}` | SDK |\n")
        return "".join(lines), EDITED

    def monorepo_diagram(text):
        if re.search(rf"^\s+{re.escape(node)}\[", text, re.M):
            return text, PRESENT
        lines = text.splitlines(keepends=True)
        sdk_edges = [i for i, l in enumerate(lines) if re.match(r"^\s+\w+\[[^\]]*\] --> SDK\s*$", l)]
        host_edges = [i for i, l in enumerate(lines) if "Host <-. Connect protocol .->" in l]
        if not sdk_edges or not host_edges:
            return text, NOT_FOUND
        indent = re.match(r"^\s*", lines[sdk_edges[-1]]).group(0)
        lines.insert(host_edges[-1] + 1, f"{indent}Host <-. Connect protocol .-> {node}\n")
        lines.insert(sdk_edges[-1] + 1, f"{indent}{node}[{title} module] --> SDK\n")
        return "".join(lines), EDITED

    apply("go.work", gowork)
    apply("scripts/build.sh", build)
    apply("scripts/check.sh", check)
    apply("scripts/check-boundaries.sh", boundaries)
    apply("justfile", justfile)
    apply("cmd/dug/main.go", help_text)
    apply("sources/README.md", sources_readme)
    apply("docs/monorepo.md", monorepo_table)
    apply("docs/monorepo.md", monorepo_diagram)

    print(f"Scaffolded sources/{name}: identity {name!r}, package {pkg}, executable dug-{name}, port {port}")
    print("Created:")
    for path in created:
        print(f"  {path}")
    print("Wiring:")
    for path, status in results:
        print(f"  {path:30} {status}")
    print("Left for you (prose; see references/wiring.md):")
    print("  docs/plugins.md       registry list, options table, Deployment port sentence, metadata paragraph")
    print("  README.md             intro registry list, '## " + title + "' section, compile-time check sentence, ordering note")
    print("  docs/monorepo.md      executable count in 'Local development', tag example list")
    print("  sources/README.md     fill the TODO cells; tick or remove the todo entry")
    print("  cmd/dug/main.go       replace the placeholder --option lines")
    print(f"Placeholders: grep -rn 'TODO({name})' sources/{name} cmd docs sources/README.md")
    print(f"Then: just test sources/{name} && just check && just build")


if __name__ == "__main__":
    main()
