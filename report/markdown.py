"""report.md: report.json's sections rendered as Markdown tables (IMPLEMENTATION_PLAN H5 step 2).

Each section is a heading; its scalar fields (nested objects flattened to dotted names) form one
Field | Value table, and every list of objects, or object of objects, inside it becomes a table of
its own, one row per item, under a sub-heading. The walk follows at most MAX_DEPTH levels of
nesting; anything deeper is one cell of compact JSON, so its recursion stays short whatever
context.json holds.
"""

import json

# report.json's sections, in order, with their headings.
SECTIONS = (
    ("context", "Context (PRF-5)"),
    ("load", "Load (PRF-1)"),
    ("results", "Results (PRF-2)"),
    ("timing", "Timing (PRF-3)"),
    ("clocks", "Clocks (T-RPT-1)"),
    ("resources", "Resources (PRF-4)"),
    ("traffic", "Traffic (TRF-1, TRF-4)"),
    ("baseline", "Baseline comparison"),
    ("correctness", "Correctness (BUD-11, BUD-11a)"),
    ("capacity", "Capacity (T-RPT-2)"),
    ("evidence_validity", "Evidence validity (§B8)"),
)

MAX_LEVEL = 6
# The nesting the tables follow (plan choice); a report's own sections nest a few levels.
MAX_DEPTH = 16


def cell(value: object) -> str:
    """One table cell: JSON spelling for null, booleans, lists and objects; integers exact,
    floats to 6 significant digits (report.json keeps them whole)."""
    if value is None:
        text = "null"
    elif isinstance(value, bool):
        text = "true" if value else "false"
    elif isinstance(value, int):
        text = str(value)
    elif isinstance(value, float):
        text = f"{value:.6g}"
    elif isinstance(value, str):
        text = value
    else:
        text = json.dumps(value, separators=(",", ":"))
    return text.replace("\\", "\\\\").replace("|", "\\|").replace("\n", " ")


def flatten(value: dict[str, object], prefix: str = "", depth: int = 0) -> list[tuple[str, object]]:
    """value's fields with nested objects spelled as dotted names, MAX_DEPTH levels down; an
    object deeper than that stays one value."""
    out: list[tuple[str, object]] = []
    for key, item in value.items():
        name = f"{prefix}{key}"
        if isinstance(item, dict) and item and depth < MAX_DEPTH:
            out.extend(flatten(item, f"{name}.", depth + 1))
        else:
            out.append((name, item))
    return out


def is_rows(value: object) -> bool:
    """Whether value renders as a table of rows: a list of objects, or an object of objects."""
    if isinstance(value, list):
        return bool(value) and all(isinstance(item, dict) for item in value)
    if isinstance(value, dict):
        return bool(value) and all(isinstance(item, dict) for item in value.values())
    return False


def holds_rows(value: object, depth: int = 0) -> bool:
    """Whether an object holds, at most MAX_DEPTH levels down, something that renders as rows."""
    if not isinstance(value, dict) or depth >= MAX_DEPTH:
        return False
    return any(is_rows(item) or holds_rows(item, depth + 1) for item in value.values())


def table(headers: list[str], rows: list[list[object]]) -> list[str]:
    out = [
        "| " + " | ".join(cell(h) for h in headers) + " |",
        "|" + "|".join("---" for _ in headers) + "|",
    ]
    out.extend("| " + " | ".join(cell(v) for v in row) + " |" for row in rows)
    out.append("")
    return out


def rows_table(value: object) -> list[str]:
    """A list of objects (or an object of objects, keyed in the first column) as one table, a
    row per item. A field that is an object with the same fields in every row spreads over one
    column per field; any other object or list is one cell of compact JSON."""
    items: list[tuple[str | None, dict[str, object]]] = []
    if isinstance(value, dict):
        items = [(str(k), v) for k, v in value.items() if isinstance(v, dict)]
    elif isinstance(value, list):
        items = [(None, v) for v in value if isinstance(v, dict)]
    keys: list[str] = []
    for _, item in items:
        keys.extend(k for k in item if k not in keys)
    spread: dict[str, list[str]] = {}
    for key in keys:
        subs = [item.get(key) for _, item in items]
        first = subs[0]
        if (
            isinstance(first, dict)
            and first
            and all(isinstance(sub, dict) and list(sub) == list(first) for sub in subs)
        ):
            spread[key] = list(first)
    headers = [] if isinstance(value, list) else ["name"]
    for key in keys:
        if key in spread:
            headers.extend(f"{key}.{sub}" for sub in spread[key])
        else:
            headers.append(key)
    rows: list[list[object]] = []
    for name, item in items:
        row: list[object] = [] if name is None else [name]
        for key in keys:
            field = item.get(key)
            if key in spread and isinstance(field, dict):
                row.extend(field.get(sub) for sub in spread[key])
            else:
                row.append(field)
        rows.append(row)
    return table(headers, rows)


def holds_rows_below(value: object) -> bool:
    """Whether an object of objects nests rows inside its items (then it renders item by item)."""
    if isinstance(value, dict):
        return any(holds_rows(item) for item in value.values())
    return False


def render_object(value: dict[str, object], level: int, path: str, depth: int = 0) -> list[str]:
    """One object: its scalar fields in a field | value table, then each part that holds rows
    under its own heading; MAX_DEPTH objects down, every field is a plain value."""
    scalars: list[tuple[str, object]] = []
    parts: list[tuple[str, object]] = []
    for key, item in value.items():
        if depth >= MAX_DEPTH:
            scalars.append((key, item))
        elif is_rows(item) or holds_rows(item):
            parts.append((key, item))
        elif isinstance(item, dict) and item:
            scalars.extend(flatten(item, f"{key}."))
        else:
            scalars.append((key, item))
    out = table(["field", "value"], [[k, v] for k, v in scalars]) if scalars else []
    for key, item in parts:
        name = f"{path}.{key}" if path else key
        out.extend(["#" * min(level + 1, MAX_LEVEL) + f" {name}", ""])
        if is_rows(item) and not holds_rows_below(item):
            out.extend(rows_table(item))
        elif isinstance(item, dict):
            out.extend(render_object(item, level + 1, name, depth + 1))
        else:
            out.extend(rows_table(item))
    return out


def render(report: dict[str, object]) -> str:
    """report.md for report (report.json's content)."""
    out = [f"# Run report {cell(report.get('run_id'))}", ""]
    for key, title in SECTIONS:
        out.extend([f"## {title}", ""])
        value = report.get(key)
        if value is None:
            out.extend(["Not applicable to this run, or not built yet.", ""])
        elif isinstance(value, dict):
            out.extend(render_object(value, 2, key))
        else:
            out.extend(table(["value"], [[value]]))
    return "\n".join(out)
